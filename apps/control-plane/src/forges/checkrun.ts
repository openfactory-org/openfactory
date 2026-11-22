/** A check run with a title, a summary and a text (core.CheckRun). */
export interface CheckRunInput {
  name: string;
  headSha: string;
  /** in_progress, completed */
  status?: string;
  /** success, failure (when completed) */
  conclusion?: string;
  title?: string;
  summary?: string;
  text?: string;
  detailsUrl?: string;
}
