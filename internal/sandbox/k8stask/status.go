package k8stask

import (
	"sort"
	"time"
	"unicode/utf8"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
)

const maxMessage = 2048

// objMeta is the part of the metadata that the backend reads.
type objMeta struct {
	Name              string            `json:"name"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	CreationTimestamp *time.Time        `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
}

type condition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason"`
	Message            string     `json:"message"`
	LastTransitionTime *time.Time `json:"lastTransitionTime"`
}

// jobObj is the part of a Job that the backend reads.
type jobObj struct {
	Metadata objMeta `json:"metadata"`
	Spec     struct {
		ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds"`
	} `json:"spec"`
	Status struct {
		Conditions     []condition `json:"conditions"`
		StartTime      *time.Time  `json:"startTime"`
		CompletionTime *time.Time  `json:"completionTime"`
	} `json:"status"`
}

type containerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running *struct {
		StartedAt *time.Time `json:"startedAt"`
	} `json:"running"`
	Terminated *terminatedState `json:"terminated"`
}

type terminatedState struct {
	ExitCode   int        `json:"exitCode"`
	Reason     string     `json:"reason"`
	Message    string     `json:"message"`
	FinishedAt *time.Time `json:"finishedAt"`
}

type containerStatus struct {
	Name  string         `json:"name"`
	State containerState `json:"state"`
}

// podObj is the part of a Pod that the backend reads.
type podObj struct {
	Metadata objMeta `json:"metadata"`
	Status   struct {
		Phase                 string            `json:"phase"`
		Reason                string            `json:"reason"`
		Message               string            `json:"message"`
		StartTime             *time.Time        `json:"startTime"`
		Conditions            []condition       `json:"conditions"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

func (j jobObj) condition(kind string) *condition {
	for i, c := range j.Status.Conditions {
		if c.Type == kind && c.Status == "True" {
			return &j.Status.Conditions[i]
		}
	}
	return nil
}

// deadlineSkew is the tolerance when the end time of a container is compared
// with the deadline of the Job. The API gives times with a precision of one
// second.
const deadlineSkew = 2 * time.Second

// endedAtDeadline reports whether a Pod ended because the Job passed its
// active deadline, before the Job has the condition Failed. The Job controller
// ends the Pods of the Job first and writes the condition after that. In that
// time, the container shows a failed exit (the signal that ends it), and
// without this check the task would end with the reason process_failed.
// at is the end time of the container, or nil if it is not known.
func (j jobObj) endedAtDeadline(at *time.Time) bool {
	d := j.Spec.ActiveDeadlineSeconds
	if d == nil || *d <= 0 || j.Status.StartTime == nil || at == nil {
		return false
	}
	limit := j.Status.StartTime.Add(time.Duration(*d) * time.Second)
	return !at.Before(limit.Add(-deadlineSkew))
}

func cut(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	s = s[:maxMessage]
	for !utf8.ValidString(s) && len(s) > 0 {
		s = s[:len(s)-1]
	}
	return s
}

// latestPod returns the newest Pod. A Job with backoffLimit 0 has one Pod.
func latestPod(pods []podObj) *podObj {
	if len(pods) == 0 {
		return nil
	}
	sorted := append([]podObj(nil), pods...)
	sort.SliceStable(sorted, func(i, k int) bool {
		a, b := sorted[i].Metadata.CreationTimestamp, sorted[k].Metadata.CreationTimestamp
		if a == nil || b == nil {
			return a == nil && b != nil
		}
		return a.Before(*b)
	})
	return &sorted[len(sorted)-1]
}

func (p *podObj) taskContainer() *containerStatus {
	for i, c := range p.Status.ContainerStatuses {
		if c.Name == taskContainer {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// disruption returns the condition DisruptionTarget of a Pod that ended or is
// being deleted. The cluster sets it when it ends a Pod on purpose: for
// example a node that is lost, a taint, a drain or a preemption.
func (p *podObj) disruption() *condition {
	if !p.ended() && p.Metadata.DeletionTimestamp == nil {
		return nil
	}
	for i, c := range p.Status.Conditions {
		if c.Type == "DisruptionTarget" && c.Status == "True" {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

func (p *podObj) ended() bool {
	return p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed"
}

var pullReasons = map[string]bool{
	"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true, "ErrImageNeverPull": true,
}

// mapStatus maps a Job and its Pods to the status of the task. It
// reads the clock only through now. It does no I/O.
func mapStatus(j jobObj, pods []podObj, now time.Time, o Options) core.TaskStatus {
	st := core.TaskStatus{
		TaskID: j.Metadata.Annotations[annTaskID],
		State:  core.TaskStarting,
	}
	p := latestPod(pods)
	if p != nil {
		st.SandboxID = p.Metadata.Name
		st.StartedAt = p.Status.StartTime
	}
	exit := func(reason core.SandboxReason, msg string, code *int, ended *time.Time) core.TaskStatus {
		st.State = core.TaskExited
		st.Reason = reason
		st.Message = cut(msg)
		st.ExitCode = code
		st.EndedAt = ended
		return st
	}
	intp := func(v int) *int { return &v }

	complete := j.condition("Complete")
	failed := j.condition("Failed")
	var jobEnded *time.Time
	switch {
	case complete != nil:
		jobEnded = j.Status.CompletionTime
		if jobEnded == nil {
			jobEnded = complete.LastTransitionTime
		}
	case failed != nil:
		jobEnded = failed.LastTransitionTime
	}

	var tc *containerStatus
	if p != nil {
		tc = p.taskContainer()
	}
	var term *terminatedState
	if tc != nil {
		term = tc.State.Terminated
	}

	// 1. The Job completed.
	if complete != nil {
		return exit(core.SandboxCompleted, "", intp(0), jobEnded)
	}
	// 2. The task was cancelled and the Pod is gone or ended.
	if j.Metadata.Annotations[annCancelled] == "true" && (p == nil || p.ended()) {
		var code *int
		if term != nil {
			code = intp(term.ExitCode)
		}
		return exit(core.SandboxCancelled, "", code, jobEnded)
	}
	// 3. The Job passed its deadline.
	if failed != nil && failed.Reason == "DeadlineExceeded" {
		return exit(core.SandboxDeadlineExceeded, failed.Message, nil, jobEnded)
	}

	// 4. Reasons that the Pod shows.
	if p != nil {
		switch p.Status.Reason {
		case "Evicted":
			return exit(core.SandboxEvicted, p.Status.Message, nil, jobEnded)
		case "DeadlineExceeded":
			// The kubelet ended the Pod at its own active deadline.
			return exit(core.SandboxDeadlineExceeded, p.Status.Message, nil, jobEnded)
		case "NodeLost", "UnexpectedAdmissionError", "Terminated":
			// Terminated is the reason of a Pod that a node shutdown ended.
			return exit(core.SandboxNodeLost, p.Status.Message, nil, jobEnded)
		}
		if c := p.disruption(); c != nil {
			switch c.Reason {
			case "PreemptionByScheduler", "EvictionByEvictionAPI":
				return exit(core.SandboxEvicted, c.Message, nil, jobEnded)
			}
			return exit(core.SandboxNodeLost, c.Message, nil, jobEnded)
		}
		if term != nil {
			ended := term.FinishedAt
			if jobEnded != nil {
				ended = jobEnded
			}
			switch {
			case term.Reason == "OOMKilled":
				return exit(core.SandboxContainerOOM, term.Message, intp(term.ExitCode), ended)
			case term.ExitCode != 0 && j.endedAtDeadline(term.FinishedAt):
				// The Job controller ended the Pod at the deadline. The
				// condition of the Job follows a moment later.
				return exit(core.SandboxDeadlineExceeded, "Job was active longer than specified deadline", intp(term.ExitCode), term.FinishedAt)
			case term.ExitCode != 0:
				return exit(core.SandboxProcessFailed, term.Message, intp(term.ExitCode), ended)
			default:
				return exit(core.SandboxCompleted, "", intp(0), ended)
			}
		}
		for _, c := range p.Status.InitContainerStatuses {
			if t := c.State.Terminated; t != nil && t.ExitCode != 0 {
				if c.Name == egress.WaitName && t.ExitCode == egress.WaitExitCode {
					// The NetworkPolicy of the Pod was not active in time. The
					// task command never ran.
					msg := t.Message
					if msg == "" {
						msg = egress.WaitMessage("the wait target", egress.DefaultWaitTimeout)
					}
					return exit(core.SandboxNetworkPolicyTimeout, "init container "+c.Name+": "+msg, intp(t.ExitCode), jobEnded)
				}
				return exit(core.SandboxProcessFailed, "init container "+c.Name+": "+t.Message, intp(t.ExitCode), jobEnded)
			}
		}
		all := append(append([]containerStatus(nil), p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
		for _, c := range all {
			w := c.State.Waiting
			if w == nil {
				continue
			}
			switch {
			case pullReasons[w.Reason] && p.Metadata.CreationTimestamp != nil &&
				now.Sub(*p.Metadata.CreationTimestamp) > o.ImagePullTimeout:
				return exit(core.SandboxImagePull, w.Reason+": "+w.Message, nil, nil)
			case w.Reason == "CreateContainerConfigError":
				return exit(core.SandboxInvalidSpec, w.Message, nil, nil)
			}
		}
		for _, c := range p.Status.Conditions {
			if c.Type != "PodScheduled" || c.Status != "False" || c.Reason != "Unschedulable" {
				continue
			}
			since := c.LastTransitionTime
			if since == nil {
				since = p.Metadata.CreationTimestamp
			}
			if since != nil && now.Sub(*since) > o.UnschedulableTimeout {
				return exit(core.SandboxUnschedulable, c.Message, nil, nil)
			}
		}
		if p.Status.Phase == "Succeeded" {
			return exit(core.SandboxCompleted, "", intp(0), jobEnded)
		}
	}

	// 5. The Job failed, or the Pod failed, and nothing above says why.
	if failed != nil || (p != nil && p.Status.Phase == "Failed") {
		msg := ""
		switch {
		case failed != nil:
			msg = failed.Message
		case p != nil:
			msg = p.Status.Message
		}
		switch {
		case p == nil, p.Status.Reason != "":
			return exit(core.SandboxUnknown, msg, nil, jobEnded)
		}
		return exit(core.SandboxProcessFailed, msg, nil, jobEnded)
	}

	// 6. The task is alive.
	if p != nil && p.Status.Phase == "Running" {
		st.State = core.TaskRunning
	}
	return st
}
