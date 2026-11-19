"use client";

import { useEffect, useRef, useState } from "react";
import { getApi, type ConsoleApi } from "@/api";

/**
 * Fetch something from the API and refetch whenever the platform reports a
 * change (a new decision, an approval, a mandate edit). Small on purpose:
 * when the real client arrives this is the place to swap in TanStack Query.
 *
 * `key` must change when the request changes (filters, ids).
 */
export function useResource<T>(key: string, load: (api: ConsoleApi) => Promise<T>) {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const loadRef = useRef(load);
  loadRef.current = load;

  useEffect(() => {
    const api = getApi();
    let alive = true;
    const run = () =>
      loadRef
        .current(api)
        .then((v) => {
          if (alive) {
            setData(v);
            setError(null);
          }
        })
        .catch((e: unknown) => alive && setError(e instanceof Error ? e.message : "Something went wrong."));
    run();
    const off = api.subscribe(run);
    return () => {
      alive = false;
      off();
    };
  }, [key]);

  return { data, error, loading: data === undefined && !error };
}

export const api = () => getApi();
