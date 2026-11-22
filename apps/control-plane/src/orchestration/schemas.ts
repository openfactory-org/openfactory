// The input schemas of the per-run factory MCP tools (tools/list). The order
// of SCHEMAS is the order of the list.

/** The names of the per-run tools. */
export type ToolName =
  | "run_agents"
  | "list_agents"
  | "follow_up_agent"
  | "send_message"
  | "ask_human"
  | "request_approval"
  | "post_update"
  | "set_stage"
  | "link_pull_request"
  | "link_issue"
  | "propose_definition_change"
  | "get_work_item"
  | "complete_work_item";

interface ToolSchema {
  description: string;
  inputSchema: Record<string, unknown>;
}

const SCHEMAS: Record<ToolName, ToolSchema> = {
  run_agents: {
    description:
      "Start one or more configured factory agents as child runs. Each child reports back to you as a follow-up message when it finishes a turn.",
    inputSchema: {
      type: "object",
      properties: {
        summary: { type: "string" },
        base_prompt: { type: "string" },
        metadata: { type: "object", additionalProperties: { type: "string" } },
        agent_run_configs: {
          type: "array",
          items: {
            type: "object",
            properties: {
              name: { type: "string" },
              agent_identity_uid: { type: "string" },
              prompt: { type: "string" },
              metadata: { type: "object", additionalProperties: { type: "string" } },
            },
            required: ["agent_identity_uid"],
          },
        },
      },
      required: ["agent_run_configs"],
    },
  },
  list_agents: {
    description: "List the factory agents you may start, with their UIDs, types and descriptions.",
    inputSchema: { type: "object", properties: {} },
  },
  follow_up_agent: {
    description: "Send a follow-up message to one of your child runs.",
    inputSchema: obj({ run_id: "string", message: "string" }, ["run_id", "message"]),
  },
  send_message: {
    description:
      "Send a message to an active run of your work item: your parent, a child, a sibling or a side run. The target gets it as an agent message from your run. Find targets in active_runs of list_agents.",
    inputSchema: obj({ run_id: "string", text: "string" }, ["run_id", "text"]),
  },
  ask_human: {
    description: "Ask the requester a question. End your turn afterwards; the answer arrives as a follow-up message.",
    inputSchema: {
      type: "object",
      properties: {
        question: { type: "string", description: "The question for the requester" },
        context: { type: "string", description: "Background needed to answer" },
        options: { type: "array", items: { type: "string" }, description: "Optional suggested answers" },
        multi_select: { type: "boolean", description: "The person can pick more than one option" },
        recommended_option: { type: "string", description: "The option you recommend. It must be one of options" },
      },
      required: ["question"],
    },
  },
  request_approval: {
    description: "Request an explicit human approval before continuing.",
    inputSchema: {
      type: "object",
      properties: {
        title: { type: "string", description: "What needs approval, e.g. 'Spec: purchase-order field'" },
        summary: { type: "string", description: "What is being approved and why" },
        links: {
          type: "array",
          items: { type: "string" },
          description: "Links to review (spec pull request, documents)",
        },
        options: { type: "array", items: { type: "string" }, description: "Decision options (default approve/reject)" },
      },
      required: ["title"],
    },
  },
  post_update: {
    description: "Post a progress update (or, with kind=handoff, the finished result) to where the work item started.",
    inputSchema: {
      type: "object",
      properties: {
        text: { type: "string", description: "Markdown update posted where the request started" },
        kind: {
          type: "string",
          description: "update (default), handoff (finished result ready for the team) or error",
        },
      },
      required: ["text"],
    },
  },
  set_stage: {
    description: "Set the work item's stage.",
    inputSchema: {
      type: "object",
      properties: {
        stage: {
          type: "string",
          description:
            "One of intake, triage, planning, awaiting_approval, building, reviewing, handoff, complete, cancelled",
        },
        note: { type: "string", description: "Why the stage changed" },
      },
      required: ["stage"],
    },
  },
  link_pull_request: {
    description: "Attach a pull request to the work item so its reviews and merge events are routed back.",
    inputSchema: {
      type: "object",
      properties: {
        url: { type: "string", description: "Pull request (merge request) URL" },
        repo: { type: "string", description: "owner/name; derived from the URL when omitted" },
        number: { type: "integer", description: "Pull request number; derived from the URL when omitted" },
        branch: { type: "string", description: "Head branch" },
        forge: { type: "string", description: "GITHUB, GITLAB or AZURE_DEVOPS; derived from the URL when omitted" },
        state: { type: "string", description: "open (default), draft, merged or closed" },
      },
      required: [],
    },
  },
  link_issue: {
    description:
      "Attach a tracker issue to the work item. Give its URL (Linear, Jira, GitHub, GitLab or an Azure DevOps work item), or a provider with a key (or a repo and a number).",
    inputSchema: obj({ url: "string", provider: "string", key: "string", repo: "string", number: "integer" }, []),
  },
  propose_definition_change: {
    description:
      "Propose a change to the managed factory definition for human review. The files are checked with the compiler and the preflight; errors come back so you can fix them. A valid proposal is stored and a reviewer sees its diff in the inbox. An approval applies it; a rejection comes back to you as a message. For a definition in git, open a pull request instead.",
    inputSchema: {
      type: "object",
      properties: {
        files: {
          type: "object",
          additionalProperties: { type: "string" },
          description: "Path relative to the definition root to the full new content of the file.",
        },
        delete: { type: "array", items: { type: "string" }, description: "Paths to remove." },
        message: { type: "string", description: "A short description of the change." },
      },
      required: ["message"],
    },
  },
  get_work_item: {
    description: "Read the work item: stage, source, pull requests, open human requests, child runs.",
    inputSchema: { type: "object", properties: {} },
  },
  complete_work_item: {
    description: "Mark the work item complete.",
    inputSchema: {
      type: "object",
      properties: { note: { type: "string", description: "Closing note, e.g. which pull request merged" } },
      required: [],
    },
  },
};

/** The tools/list answer. */
export const TOOLS: readonly ({ name: ToolName } & ToolSchema)[] = Object.entries(SCHEMAS).map(([name, s]) => ({
  name: name as ToolName,
  ...s,
}));

/** Whether name is one of the per-run tools. */
export function isToolName(name: string): name is ToolName {
  return Object.hasOwn(SCHEMAS, name);
}

function obj(props: Record<string, string>, required: string[]): Record<string, unknown> {
  return {
    type: "object",
    properties: Object.fromEntries(Object.entries(props).map(([k, t]) => [k, { type: t }])),
    required,
  };
}
