import { useEffect, useRef, useState } from "react";

export type StreamStatus = "connecting" | "open" | "closed" | "error";

export type StreamHandlers = Record<string, (data: unknown, ev: MessageEvent) => void>;

/**
 * Subscribes to a Server-Sent Events URL (same origin, cookies included).
 * Handlers are keyed by event name; JSON data is parsed before dispatch.
 * Pass url = null to stay disconnected. A handler may return after the
 * server ends the stream ("done"): call `close()` to stop reconnects.
 */
export function useEventStream(url: string | null, handlers: StreamHandlers): { status: StreamStatus; close: () => void } {
  const [status, setStatus] = useState<StreamStatus>(url ? "connecting" : "closed");
  const handlersRef = useRef(handlers);
  handlersRef.current = handlers;
  const sourceRef = useRef<EventSource | null>(null);

  useEffect(() => {
    if (!url || typeof EventSource === "undefined") {
      setStatus("closed");
      return;
    }
    const es = new EventSource(url, { withCredentials: true });
    sourceRef.current = es;
    setStatus("connecting");
    es.onopen = () => setStatus("open");
    es.onerror = () => setStatus(es.readyState === EventSource.CLOSED ? "closed" : "error");
    const names = Object.keys(handlersRef.current);
    const listeners = names.map((name) => {
      const fn = (ev: Event) => {
        const me = ev as MessageEvent;
        let data: unknown = me.data;
        if (typeof me.data === "string") {
          try {
            data = JSON.parse(me.data);
          } catch {
            /* keep raw text */
          }
        }
        handlersRef.current[name]?.(data, me);
      };
      es.addEventListener(name, fn);
      return [name, fn] as const;
    });
    return () => {
      listeners.forEach(([n, fn]) => es.removeEventListener(n, fn));
      es.close();
      sourceRef.current = null;
    };
    // Handler identity changes must not reconnect; only the set of names matters.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [url, Object.keys(handlers).sort().join(",")]);

  return {
    status,
    close: () => {
      sourceRef.current?.close();
      setStatus("closed");
    },
  };
}
