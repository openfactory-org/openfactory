package backend

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/machine/image"
	"github.com/openfactory-org/openfactory/internal/machine/mmds"
	"github.com/openfactory-org/openfactory/internal/machine/vmnet"
)

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// document builds the MMDS session document.
func (b *Backend) document(tk *task, img image.Image, n vmnet.VMNet, cacheKeys []string) mmds.Document {
	t := tk.full
	entropy := make([]byte, 64)
	_, _ = rand.Read(entropy)
	p := mmds.Payload{
		V: mmds.Version, TaskID: t.TaskID,
		Hostname:          "of-" + strings.TrimPrefix(tk.rec.VMID[strings.LastIndex(tk.rec.VMID, "-")+1:], "-"),
		ClockUnixNs:       b.o.Now().UnixNano(),
		Entropy:           base64.StdEncoding.EncodeToString(entropy),
		MachineID:         randHex(16),
		User:              resolveUser(t.Platform.User, img.User),
		Image:             &mmds.Image{Env: img.Env, WorkingDir: img.WorkingDir},
		Env:               map[string]string{},
		Disks:             &mmds.Disks{Image: "image", Scratch: "scratch"},
		MaxSessionSeconds: t.MaxSessionSeconds,
	}
	for k, v := range t.Env {
		p.Env[k] = v
	}
	host := n.HostIP.String()
	nw := &mmds.Network{Mode: n.Mode.String(), Address: n.GuestPrefix(), Gateway: host, MTU: 1500, DNS: []string{}}
	if n.Mode == vmnet.Unrestricted {
		nw.DNS = []string{host}
	} else {
		nw.ProxyURL = "http://" + host + ":" + strconv.Itoa(vmnet.ProxyPort)
		nw.NoProxy = "localhost,127.0.0.1"
		nw.EgressReportURL = "http://" + host + ":" + strconv.Itoa(vmnet.ReportPort) + "/blocked"
	}
	p.Network = nw
	for i, k := range cacheKeys {
		p.Disks.Cache = append(p.Disks.Cache, mmds.CacheDisk{Drive: "cache-" + strconv.Itoa(i), Key: k})
	}
	return mmds.Document{OpenFactory: p}
}

// resolveUser picks the runner user: a numeric platform.user, else the
// image user (numeric, or a name that vm-init resolves), else 1001.
func resolveUser(platformUser, imageUser string) *mmds.User {
	u := &mmds.User{UID: 1001, GID: 1001, Home: "/home/agent"}
	pick := strings.TrimSpace(platformUser)
	if pick == "" || pick == "image" {
		pick = strings.TrimSpace(imageUser)
	}
	if pick == "" {
		return u
	}
	uidS, gidS, hasGID := strings.Cut(pick, ":")
	if uid, err := strconv.Atoi(uidS); err == nil && uid >= 0 {
		u.UID, u.GID = uid, uid
		if gid, err := strconv.Atoi(gidS); hasGID && err == nil && gid >= 0 {
			u.GID = gid
		}
		if uid == 0 {
			u.Home = "/root"
		}
		return u
	}
	u.Name = uidS
	return u
}
