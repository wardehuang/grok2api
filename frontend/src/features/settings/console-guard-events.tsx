import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Spinner } from "@/components/ui/spinner";
import { getRequestAudits, type AuditDTO } from "@/features/audits/request-audits-api";
import { RequestAuditDetailDialog } from "@/features/audits/request-audit-detail-dialog";

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
  const [selectedEvent, setSelectedEvent] = useState<AuditDTO | null>(null);

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
        <button key={event.id} type="button" title={t("audits.viewDetails")} aria-label={t("audits.viewDetails")} className="flex w-full flex-wrap items-center gap-x-3 gap-y-1 rounded-md border px-3 py-2 text-left text-xs transition-colors hover:bg-accent/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50" onClick={() => setSelectedEvent(event)}>
          <Badge variant="secondary" className="font-mono">{event.accountName || event.accountId || "?"}</Badge>
          {event.modelUpstreamModel ? <span className="text-muted-foreground">{event.modelUpstreamModel}</span> : null}
          <span className="text-muted-foreground">{t("qualityGuard.consoleGuard.outputTokens", { count: event.consoleGuard?.outputTokens ?? event.outputTokens })} · {t("qualityGuard.consoleGuard.reasoningTokens", { count: event.consoleGuard?.reasoningTokens ?? event.reasoningTokens })}</span>
          {event.consoleGuard ? <Badge variant="outline">{event.consoleGuard.hasThinking ? t("qualityGuard.consoleGuard.thinkingDetected") : t("qualityGuard.consoleGuard.thinkingMissing")}</Badge> : null}
          <span className="text-muted-foreground">{t("qualityGuard.consoleGuard.duration", { value: (event.durationMs / 1000).toFixed(1) })}</span>
          <time className="ml-auto text-muted-foreground" dateTime={event.createdAt}>
            {new Date(event.createdAt).toLocaleString()}
          </time>
        </button>
      ))}
      <RequestAuditDetailDialog audit={selectedEvent} open={selectedEvent !== null} onOpenChange={(open) => { if (!open) setSelectedEvent(null); }} />
    </div>
  );
}
