import { useMutation, useQueryClient, type QueryKey } from "@tanstack/react-query";
import { errorMessage } from "../api/client";
import { useToast } from "../components/Toast";

/**
 * Wraps useMutation with the dashboard conventions: invalidate the given query
 * keys on success, show a success toast, and show API errors as toasts unless
 * the caller renders them itself (quietErrors).
 */
export function useMutate<TVars, TData>(
  fn: (vars: TVars) => Promise<TData>,
  opts: {
    invalidate?: QueryKey[];
    success?: string | ((data: TData, vars: TVars) => string | undefined);
    onSuccess?: (data: TData, vars: TVars) => void;
    quietErrors?: boolean;
    /** A button on the success toast, for example Undo. */
    action?: (data: TData, vars: TVars) => { label: string; onClick: () => void } | undefined;
  } = {},
) {
  const qc = useQueryClient();
  const toast = useToast();
  return useMutation({
    mutationFn: fn,
    onSuccess: async (data, vars) => {
      await Promise.all((opts.invalidate ?? []).map((k) => qc.invalidateQueries({ queryKey: k })));
      const msg = typeof opts.success === "function" ? opts.success(data, vars) : opts.success;
      if (msg) toast.show(msg, "success", opts.action?.(data, vars));
      opts.onSuccess?.(data, vars);
    },
    onError: (err) => {
      if (!opts.quietErrors) toast.show(errorMessage(err), "error");
    },
  });
}
