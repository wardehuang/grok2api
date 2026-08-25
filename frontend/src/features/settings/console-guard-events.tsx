import { useInfiniteQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Spinner } from "@/components/ui/spinner";
import { getRequestAudits, type AuditDTO } from "@/features/audits/request-audits-api";
import { RequestAuditDetailDialog } from "@/features/audits/request-audit-detail-dialog";

type ConsoleGuardEvent = AuditDTO;

function isDegradedEvent(event: ConsoleGuardEvent): boolean {
  return event.consoleGuard?.degraded ?? (event.consoleGuard?.verdict === "withhold" || event.errorCode === "console_guard_degraded");
}

export function useConsoleGuardEvents(enabled: boolean) {
  return useInfiniteQuery({
    queryKey: ["console-guard-events", "grok_console"],
    enabled,
    refetchInterval: 30_000,
    queryFn: ({ pageParam }) => getRequestAudits({ period: "24h", pageSize: 20, cursor: pageParam || undefined, provider: "grok_console", consoleGuardOnly: true }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => (lastPage.hasMore && lastPage.nextCursor ? lastPage.nextCursor : undefined),
  });
}

export function ConsoleGuardEvents() {
  const { t } = useTranslation();
  const eventsQuery = useConsoleGuardEvents(true);
  const [selectedEvent, setSelectedEvent] = useState<AuditDTO | null>(null);
  const items = useMemo(() => Array.from(new Map((eventsQuery.data?.pages ?? []).flatMap((page) => page.items).map((event) => [event.requestId || event.id, event])).values()), [eventsQuery.data]);

  if (eventsQuery.isPending) {
    return <div className="flex min-h-16 items-center justify-center"><Spinner /></div>;
  }
  if (eventsQuery.isError) {
    return <p className="text-sm text-muted-foreground">{t("qualityGuard.consoleGuard.eventsFailed")}</p>;
  }
  if (items.length === 0) {
    return <p className="text-sm text-muted-foreground">{t("qualityGuard.consoleGuard.eventsEmpty")}</p>;
  }
  return (
    <div className="space-y-2">
      {items.map((event) => (
        (() => {
          const degraded = isDegradedEvent(event);
          return <button key={event.id} type="button" title={t("audits.viewDetails")} aria-label={t("audits.viewDetails")} className={`flex w-full flex-wrap items-center gap-x-3 gap-y-1 rounded-md border px-3 py-2 text-left text-xs transition-colors hover:bg-accent/50 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 ${degraded ? "border-amber-500/50 bg-amber-500/5" : "border-emerald-500/50 bg-emerald-500/5"}`} onClick={() => setSelectedEvent(event)}>
          <Badge variant="secondary" className="font-mono">{event.accountName || event.accountId || "?"}</Badge>
          {event.modelUpstreamModel ? <span className="text-muted-foreground">{event.modelUpstreamModel}</span> : null}
          <span className="text-muted-foreground">{t("qualityGuard.consoleGuard.outputTokens", { count: event.consoleGuard?.outputTokens ?? event.outputTokens })} · {t("qualityGuard.consoleGuard.reasoningTokens", { count: event.consoleGuard?.reasoningTokens ?? event.reasoningTokens })}</span>
          <Badge variant="outline" className={degraded ? "border-amber-500/60 text-amber-700 dark:text-amber-300" : "border-emerald-500/60 text-emerald-700 dark:text-emerald-300"}>{degraded ? t("qualityGuard.consoleGuard.degradedStatus") : t("qualityGuard.consoleGuard.normalStatus")}</Badge>
          {event.consoleGuard ? <Badge variant="outline">{event.consoleGuard.hasThinking ? t("qualityGuard.consoleGuard.thinkingDetected") : t("qualityGuard.consoleGuard.thinkingMissing")}</Badge> : null}
          <span className="text-muted-foreground">{t("qualityGuard.consoleGuard.duration", { value: (event.durationMs / 1000).toFixed(1) })}</span>
          <time className="ml-auto text-muted-foreground" dateTime={event.createdAt}>
            {new Date(event.createdAt).toLocaleString()}
          </time>
          </button>;
        })()
      ))}
      {eventsQuery.hasNextPage ? (
        <Button type="button" variant="secondary" size="sm" disabled={eventsQuery.isFetchingNextPage} onClick={() => void eventsQuery.fetchNextPage()} className="w-full">
          {eventsQuery.isFetchingNextPage ? <Spinner className="size-4" /> : t("qualityGuard.consoleGuard.eventsLoadMore")}
        </Button>
      ) : null}
      <RequestAuditDetailDialog audit={selectedEvent} open={selectedEvent !== null} onOpenChange={(open) => { if (!open) setSelectedEvent(null); }} />
    </div>
  );
}
