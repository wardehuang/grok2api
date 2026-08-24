import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Spinner } from "@/components/ui/spinner";
import { getRequestAudits, type AuditDTO } from "@/features/audits/request-audits-api";

type ConsoleGuardEvent = AuditDTO;

export function useConsoleGuardEvents(enabled: boolean) {
  return useQuery({
    queryKey: ["console-guard-events"],
    enabled,
    refetchInterval: 30_000,
    queryFn: () => getRequestAudits({ period: "24h", pageSize: 20, errorCode: "console_guard_degraded" }),
  });
}

export function ConsoleGuardEvents() {
  const { t } = useTranslation();
  const eventsQuery = useConsoleGuardEvents(true);

  if (eventsQuery.isPending) {
    return <div className="flex min-h-16 items-center justify-center"><Spinner /></div>;
  }
  if (eventsQuery.isError) {
    return <p className="text-sm text-muted-foreground">{t("qualityGuard.consoleGuard.eventsFailed")}</p>;
  }
  const items: ConsoleGuardEvent[] = eventsQuery.data.items;
  if (items.length === 0) {
    return <p className="text-sm text-muted-foreground">{t("qualityGuard.consoleGuard.eventsEmpty")}</p>;
  }
  return (
    <div className="space-y-2">
      {items.map((event) => (
        <div key={event.id} className="flex flex-wrap items-center gap-x-3 gap-y-1 rounded-md border px-3 py-2 text-xs">
          <Badge variant="secondary" className="font-mono">{event.accountName || event.accountId || "?"}</Badge>
          {event.modelUpstreamModel ? <span className="text-muted-foreground">{event.modelUpstreamModel}</span> : null}
          <span className="text-muted-foreground">{t("qualityGuard.consoleGuard.outputTokens", { count: event.outputTokens })} · {t("qualityGuard.consoleGuard.reasoningTokens", { count: event.reasoningTokens })}</span>
          <span className="text-muted-foreground">{t("qualityGuard.consoleGuard.duration", { value: (event.durationMs / 1000).toFixed(1) })}</span>
          <time className="ml-auto text-muted-foreground" dateTime={event.createdAt}>
            {new Date(event.createdAt).toLocaleString()}
          </time>
        </div>
      ))}
    </div>
  );
}
