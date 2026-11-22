// Event attribute keys that plugins set on PluginEvent.attributes. Port of
// internal/forge/eventattr (the names are part of the trigger match contract).

export const EVENT_ATTR = {
  Repo: "repo",
  SourceRepo: "source_repo",
  Labels: "labels",
  AddedLabels: "added_labels",
  Assignees: "assignees",
  Author: "author",
  Mentioned: "mentioned",
  BaseBranch: "base_branch",
  Branch: "branch",
  PRNumber: "pr_number",
  Paths: "paths",
  Conclusion: "conclusion",
  Action: "action",
  WorkItemType: "work_item_type",
  Text: "text",
  ActorIsBot: "actor_is_bot",
  ActorIsSelf: "actor_is_self",
  Alias: "alias",
  Draft: "draft",
  HeadBranch: "head_branch",
  HeadSHA: "head_sha",
  ThreadRef: "thread_ref",
} as const;

export const PROVIDER_GITLAB = "gitlab";
export const PROVIDER_AZURE_DEVOPS = "azure_devops";
