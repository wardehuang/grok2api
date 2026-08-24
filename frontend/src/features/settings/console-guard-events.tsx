import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Spinner } from "@/components/ui/spinner";
import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isNumber, isOptional, isString } from "@/shared/api/decoder";

type ConsoleGuardEventDTO = {
  id: string;
  requestId: string;
  accountId?: string;
  accountName: string;
  modelUpstreamModel?: string;
  outputTokens: number;
  reasoningTokens: number;
  durationMs: number;
  createdAt: string;
};

const decodeConsoleGuardEvents = createObjectDecoder<{ items: ConsoleGuardEventDTO[] }>("console guard events", {
  items: isArrayOf(hasShape({
    id: isString,
    requestId: isString,
    accountId: isOptional(isString),
    accountName: isString,
    modelUpstreamModel: isOptional(isString),
    outputTokens: isNumber,
    reasoningTokens: isNumber,
    durationMs: isNumber,
    createdAt: isString,
  })),
});

export function useConsoleGuardEvents(enabled: boolean) {
  return useQuery({
    queryKey: ["console-guard-events"],
    enabled,
    refetchInterval: 30_000,
    queryFn: async (): Promise<{ items: ConsoleGuardEventDTO[] }> => {
      const raw = await apiRequest("/api/admin/v1/request-audits?pagination=cursor&pageSize=20&errorCode=console_guard_degraded&period=24h", {}, (value: unknown) => value);
      const payload = raw as { items?: unknown[] };
      if (!Array.isArray(payload.items)) return { items: [] };
      return decodeConsoleGuardEvents({ items: payload.items });
    },
  });
}

export function ConsoleGuardEvents() {
  const { t } = useTranslation();
  const eventsQuery = useConsoleGuardEvents(true);

  if (eventsQuery.isPending) {
    return <div className="flex min-h-16 items-center justify-center"><Spinner /></div>;
  }
  if (eventsQuery.isError) {
    return <p className="text-sm text-muted-foreground">{t("settings.accounts.consoleGuardEventsFailed")}</p>;
  }
  const items = eventsQuery.data?.items ?? [];
  if (items.length === 0) {
    return <p className="text-sm text-muted-foreground">{t("settings.accounts.consoleGuardEventsEmpty")}</p>;
  }
  return (
    <div className="space-y-2">
      {items.map((event) => (
        <div key={event.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border px-3 py-2 text-xs">
          <Badge variant="secondary" className="font-mono">{event.accountName || event.accountId || "?"}</Badge>
          {event.modelUpstreamModel ? <span className="text-muted-foreground">{event.modelUpstreamModel}</span> : null}
          <span className="text-muted-foreground">out {event.outputTokens} · reason {event.reasoningTokens}</span>
          <span className="text-muted-foreground">{(event.durationMs / 1000).toFixed(1)}s</span>
          <time className="ml-auto text-muted-foreground" dateTime={event.createdAt}>
            {new Date(event.createdAt).toLocaleString()}
          </time>
        </div>
      ))}
    </div>
  );
}
