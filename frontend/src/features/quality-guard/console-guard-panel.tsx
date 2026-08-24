import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { getSettings, updateSettings } from "@/features/settings/settings-api";
import { ConsoleGuardEvents } from "@/features/settings/console-guard-events";
import { ErrorState } from "@/shared/components/data-state";

export function ConsoleGuardPanel() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const settingsQuery = useQuery({ queryKey: ["settings"], queryFn: getSettings });
  const toggleMutation = useMutation({
    mutationFn: (enabled: boolean) => {
      const snapshot = settingsQuery.data!;
      return updateSettings(snapshot.revision, {
        ...snapshot.config,
        consoleGuard: { enabled },
      });
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData(["settings"], snapshot);
      toast.success(t("qualityGuard.consoleGuard.saved"));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("qualityGuard.consoleGuard.saveFailed")),
  });

  if (settingsQuery.isError) return <ErrorState message={settingsQuery.error.message} onRetry={() => void settingsQuery.refetch()} />;
  if (settingsQuery.isPending) return <div className="flex min-h-32 items-center justify-center"><Spinner /></div>;

  const enabled = settingsQuery.data!.config.consoleGuard.enabled;
  return (
    <div className="space-y-6">
      <section className="overflow-hidden rounded-lg bg-card">
        <div className="flex flex-col gap-4 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
          <div className="min-w-0">
            <h2 className="text-sm font-medium">{t("qualityGuard.consoleGuard.title")}</h2>
            <p className="mt-1 max-w-3xl text-xs text-muted-foreground">{t("qualityGuard.consoleGuard.enabledHelp")}</p>
          </div>
          <div className="flex shrink-0 items-center gap-3">
            <Badge variant={enabled ? "default" : "secondary"}>
              {enabled ? t("qualityGuard.consoleGuard.enabledStatus") : t("qualityGuard.consoleGuard.disabledStatus")}
            </Badge>
            <Switch
              checked={enabled}
              disabled={toggleMutation.isPending}
              onCheckedChange={(checked) => toggleMutation.mutate(checked)}
              aria-label={t("qualityGuard.consoleGuard.enabled")}
            />
          </div>
        </div>
      </section>

      <section className="overflow-hidden rounded-lg bg-card">
        <div className="border-b px-4 py-4 sm:px-5">
          <h2 className="text-sm font-medium">{t("qualityGuard.consoleGuard.eventsTitle")}</h2>
        </div>
        <div className="p-4 sm:p-5">
          <ConsoleGuardEvents />
        </div>
      </section>
    </div>
  );
}
