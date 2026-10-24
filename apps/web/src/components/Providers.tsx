"use client";

// QueryClientProvider for the authenticated shell (issue #32): the
// requisition list is the first client-fetched screen; TanStack Query owns
// fetching/caching with the filter tuple as the query key. One client per
// browser session (SSR-safe via the lazy initializer), created in the
// client-only Providers component so server components stay unwrapped.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";

export default function Providers({ children }: { children: ReactNode }) {
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            staleTime: 15_000,
            retry: 1,
            refetchOnWindowFocus: false,
          },
        },
      }),
  );
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}
