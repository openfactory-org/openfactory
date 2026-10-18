import { useEffect, useId, useState, type FormEvent } from "react";
import { Link, useNavigate } from "react-router-dom";
import { fpath, isSubmitChord, MOD, useDispatch, useFactories } from "../lib/queries";
import { Dialog } from "./overlays";
import { Button, Keys, Skeleton } from "./ui";

/**
 * The form that sends a request to a factory's foreman. It creates a work item
 * and opens it. Used by the "New work item" dialog and the /chat page.
 */
export function ComposerForm({ factoryID, onSent, autoFocus }: { factoryID?: string; onSent?: () => void; autoFocus?: boolean }) {
  const factories = useFactories();
  const navigate = useNavigate();
  const id = useId();
  const [picked, setPicked] = useState(factoryID ?? "");
  const [prompt, setPrompt] = useState("");
  useEffect(() => {
    if (factoryID) setPicked(factoryID);
  }, [factoryID]);
  const dispatch = useDispatch((r, fid) => {
    onSent?.();
    navigate(r.work_item_id ? fpath(fid, `work/${r.work_item_id}`) : `/runs/${r.run_id}`);
  });
  const fid = picked || factories.data?.[0]?.id || "";

  function submit(e?: FormEvent) {
    e?.preventDefault();
    if (prompt.trim() && fid && !dispatch.isPending) dispatch.mutate({ fid, prompt: prompt.trim() });
  }

  if (factories.isLoading) return <Skeleton rows={3} />;
  if ((factories.data ?? []).length === 0) {
    return (
      <p className="muted">
        No factories yet.{" "}
        <Link to="/factories/new" onClick={onSent}>
          Create a factory
        </Link>{" "}
        first.
      </p>
    );
  }
  return (
    <form className="composer" onSubmit={submit}>
      <label className="sr-only" htmlFor={`${id}-prompt`}>
        Request
      </label>
      <textarea
        id={`${id}-prompt`}
        className="composer-input"
        rows={5}
        required
        autoFocus={autoFocus}
        value={prompt}
        onChange={(e) => setPrompt(e.target.value)}
        onKeyDown={(e) => isSubmitChord(e) && submit()}
        placeholder="Describe one small, verifiable change. The first line becomes the title."
      />
      <div className="composer-foot">
        <label className="sr-only" htmlFor={`${id}-factory`}>
          Factory
        </label>
        <select id={`${id}-factory`} value={fid} onChange={(e) => setPicked(e.target.value)}>
          {(factories.data ?? []).map((f) => (
            <option key={f.id} value={f.id}>
              {f.name}
            </option>
          ))}
        </select>
        <span className="grow" />
        <Button type="submit" variant="primary" busy={dispatch.isPending} disabled={!prompt.trim()}>
          Send to foreman <Keys k={`${MOD} ↵`} />
        </Button>
      </div>
    </form>
  );
}

export function ComposerDialog({ open, onOpenChange, factoryID }: { open: boolean; onOpenChange: (o: boolean) => void; factoryID?: string }) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange} title="New work item" className="dialog-wide dialog-top">
      <ComposerForm factoryID={factoryID} onSent={() => onOpenChange(false)} autoFocus />
    </Dialog>
  );
}
