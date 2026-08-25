import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Spinner } from "@/components/ui/spinner";
import { Switch } from "@/components/ui/switch";
import { getConsoleGuardProxyFilePreview, getSettings, updateSettings } from "@/features/settings/settings-api";
import { ConsoleGuardEvents } from "@/features/settings/console-guard-events";
import { ErrorState } from "@/shared/components/data-state";

export function ConsoleGuardPanel() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const settingsQuery = useQuery({ queryKey: ["settings"], queryFn: getSettings });
  const [softTPS, setSoftTPS] = useState<number | "">(500);
  const [hardTPS, setHardTPS] = useState<number | "">(1000);
  const [firstTokenThresholdMS, setFirstTokenThresholdMS] = useState<number | "">(5000);
  const [generationWindowThresholdMS, setGenerationWindowThresholdMS] = useState<number | "">(1250);
  const [minOutputReasoningTokens, setMinOutputReasoningTokens] = useState<number | "">(300);
  const [recordNonDegradedEvents, setRecordNonDegradedEvents] = useState(true);
  const [degradedEgressNodeFilePath, setDegradedEgressNodeFilePath] = useState("");
  const [proxyPreviewOpen, setProxyPreviewOpen] = useState(false);
  useEffect(() => {
    if (!settingsQuery.data) return;
    setSoftTPS(settingsQuery.data.config.consoleGuard.softTPS);
    setHardTPS(settingsQuery.data.config.consoleGuard.hardTPS);
    setFirstTokenThresholdMS(settingsQuery.data.config.consoleGuard.firstTokenThresholdMS);
    setGenerationWindowThresholdMS(settingsQuery.data.config.consoleGuard.generationWindowThresholdMS);
    setMinOutputReasoningTokens(settingsQuery.data.config.consoleGuard.minOutputReasoningTokens);
    setRecordNonDegradedEvents(settingsQuery.data.config.consoleGuard.recordNonDegradedEvents);
    setDegradedEgressNodeFilePath(settingsQuery.data.config.consoleGuard.degradedEgressNodeFilePath);
  }, [settingsQuery.data]);
  const toggleMutation = useMutation({
    mutationFn: (enabled: boolean) => {
      const snapshot = settingsQuery.data!;
      return updateSettings(snapshot.revision, {
        ...snapshot.config,
        consoleGuard: { ...snapshot.config.consoleGuard, enabled },
      });
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData(["settings"], snapshot);
      toast.success(t("qualityGuard.consoleGuard.saved"));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("qualityGuard.consoleGuard.saveFailed")),
  });
  const thresholdMutation = useMutation({
    mutationFn: () => {
      const snapshot = settingsQuery.data!;
      return updateSettings(snapshot.revision, {
        ...snapshot.config,
        consoleGuard: { ...snapshot.config.consoleGuard, softTPS: Number(softTPS), hardTPS: Number(hardTPS), firstTokenThresholdMS: Number(firstTokenThresholdMS), generationWindowThresholdMS: Number(generationWindowThresholdMS), minOutputReasoningTokens: Number(minOutputReasoningTokens) },
      });
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData(["settings"], snapshot);
      toast.success(t("qualityGuard.consoleGuard.thresholdsSaved"));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("qualityGuard.consoleGuard.thresholdsSaveFailed")),
  });
  const recordEventsMutation = useMutation({
    mutationFn: (checked: boolean) => {
      const snapshot = settingsQuery.data!;
      return updateSettings(snapshot.revision, {
        ...snapshot.config,
        consoleGuard: { ...snapshot.config.consoleGuard, recordNonDegradedEvents: checked },
      });
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData(["settings"], snapshot);
      toast.success(t("qualityGuard.consoleGuard.saved"));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("qualityGuard.consoleGuard.saveFailed")),
  });
  const proxyFilePathMutation = useMutation({
    mutationFn: () => {
      const snapshot = settingsQuery.data!;
      return updateSettings(snapshot.revision, {
        ...snapshot.config,
        consoleGuard: { ...snapshot.config.consoleGuard, degradedEgressNodeFilePath: degradedEgressNodeFilePath.trim() },
      });
    },
    onSuccess: (snapshot) => {
      queryClient.setQueryData(["settings"], snapshot);
      toast.success(t("qualityGuard.consoleGuard.proxyFileSaved"));
    },
    onError: (error) => toast.error(error instanceof Error ? error.message : t("qualityGuard.consoleGuard.proxyFileSaveFailed")),
  });
  const proxyPreviewQuery = useQuery({
    queryKey: ["consoleGuardProxyFilePreview"],
    queryFn: getConsoleGuardProxyFilePreview,
    enabled: proxyPreviewOpen,
  });

  if (settingsQuery.isError) return <ErrorState message={settingsQuery.error.message} onRetry={() => void settingsQuery.refetch()} />;
  if (settingsQuery.isPending) return <div className="flex min-h-32 items-center justify-center"><Spinner /></div>;

  const enabled = settingsQuery.data!.config.consoleGuard.enabled;
  const savedSoftTPS = settingsQuery.data!.config.consoleGuard.softTPS;
  const savedHardTPS = settingsQuery.data!.config.consoleGuard.hardTPS;
  const savedFirstTokenThresholdMS = settingsQuery.data!.config.consoleGuard.firstTokenThresholdMS;
  const savedGenerationWindowThresholdMS = settingsQuery.data!.config.consoleGuard.generationWindowThresholdMS;
  const savedMinOutputReasoningTokens = settingsQuery.data!.config.consoleGuard.minOutputReasoningTokens;
  const savedDegradedEgressNodeFilePath = settingsQuery.data!.config.consoleGuard.degradedEgressNodeFilePath;
  const thresholdsValid = typeof softTPS === "number" && Number.isFinite(softTPS) && softTPS >= 1 && softTPS <= 10_000 && typeof hardTPS === "number" && Number.isFinite(hardTPS) && hardTPS > softTPS && hardTPS <= 10_000 && typeof firstTokenThresholdMS === "number" && Number.isInteger(firstTokenThresholdMS) && firstTokenThresholdMS >= 1 && typeof generationWindowThresholdMS === "number" && Number.isInteger(generationWindowThresholdMS) && generationWindowThresholdMS >= 1 && typeof minOutputReasoningTokens === "number" && Number.isInteger(minOutputReasoningTokens) && minOutputReasoningTokens >= 1;
  const thresholdsDirty = softTPS !== savedSoftTPS || hardTPS !== savedHardTPS || firstTokenThresholdMS !== savedFirstTokenThresholdMS || generationWindowThresholdMS !== savedGenerationWindowThresholdMS || minOutputReasoningTokens !== savedMinOutputReasoningTokens;
  const proxyFilePathValid = degradedEgressNodeFilePath.trim().length > 0 && degradedEgressNodeFilePath.trim().length <= 4096;
  const proxyFilePathDirty = degradedEgressNodeFilePath !== savedDegradedEgressNodeFilePath;
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
        <div className="flex flex-col gap-4 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
          <div className="min-w-0">
            <h2 className="text-sm font-medium">{t("qualityGuard.consoleGuard.recordNonDegradedEvents")}</h2>
            <p className="mt-1 max-w-3xl text-xs text-muted-foreground">{t("qualityGuard.consoleGuard.recordNonDegradedEventsHelp")}</p>
          </div>
          <Switch
            checked={recordNonDegradedEvents}
            disabled={recordEventsMutation.isPending}
            onCheckedChange={(checked) => recordEventsMutation.mutate(checked)}
            aria-label={t("qualityGuard.consoleGuard.recordNonDegradedEvents")}
          />
        </div>
      </section>

      <section className="overflow-hidden rounded-lg bg-card">
        <div className="border-b px-4 py-4 sm:px-5">
          <h2 className="text-sm font-medium">{t("qualityGuard.consoleGuard.thresholdsTitle")}</h2>
          <p className="mt-1 text-xs text-muted-foreground">{t("qualityGuard.consoleGuard.thresholdsHelp")}</p>
        </div>
        <div className="space-y-4 p-4 sm:p-5">
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <label className="space-y-1.5 text-sm">
              <span>{t("qualityGuard.consoleGuard.softTPS")}</span>
              <Input type="number" min={1} max={10_000} step="any" value={softTPS} onChange={(event) => setSoftTPS(event.currentTarget.value === "" ? "" : event.currentTarget.valueAsNumber)} />
            </label>
            <label className="space-y-1.5 text-sm">
              <span>{t("qualityGuard.consoleGuard.hardTPS")}</span>
              <Input type="number" min={1} max={10_000} step="any" value={hardTPS} onChange={(event) => setHardTPS(event.currentTarget.value === "" ? "" : event.currentTarget.valueAsNumber)} />
            </label>
            <label className="space-y-1.5 text-sm">
              <span>{t("qualityGuard.consoleGuard.firstTokenThresholdMS")}</span>
              <Input type="number" min={1} step={1} value={firstTokenThresholdMS} onChange={(event) => setFirstTokenThresholdMS(event.currentTarget.value === "" ? "" : event.currentTarget.valueAsNumber)} />
            </label>
            <label className="space-y-1.5 text-sm">
              <span>{t("qualityGuard.consoleGuard.generationWindowThresholdMS")}</span>
              <Input type="number" min={1} step={1} value={generationWindowThresholdMS} onChange={(event) => setGenerationWindowThresholdMS(event.currentTarget.value === "" ? "" : event.currentTarget.valueAsNumber)} />
            </label>
            <label className="space-y-1.5 text-sm">
              <span>{t("qualityGuard.consoleGuard.minOutputReasoningTokens")}</span>
              <Input type="number" min={1} step={1} value={minOutputReasoningTokens} onChange={(event) => setMinOutputReasoningTokens(event.currentTarget.value === "" ? "" : event.currentTarget.valueAsNumber)} />
            </label>
          </div>
          {!thresholdsValid ? <p className="text-xs text-destructive">{t("qualityGuard.consoleGuard.thresholdsInvalid")}</p> : null}
          <Button type="button" size="sm" disabled={!thresholdsValid || !thresholdsDirty || thresholdMutation.isPending || toggleMutation.isPending} onClick={() => thresholdMutation.mutate()}>
            {t("qualityGuard.consoleGuard.saveThresholds")}
          </Button>
        </div>
      </section>

      <section className="overflow-hidden rounded-lg bg-card">
        <div className="border-b px-4 py-4 sm:px-5">
          <h2 className="text-sm font-medium">{t("qualityGuard.consoleGuard.proxyFileTitle")}</h2>
          <p className="mt-1 text-xs text-muted-foreground">{t("qualityGuard.consoleGuard.proxyFileHelp")}</p>
        </div>
        <div className="space-y-3 p-4 sm:p-5">
          <div className="flex flex-col gap-2 sm:flex-row sm:items-end">
            <label className="min-w-0 flex-1 space-y-1.5 text-sm">
              <span>{t("qualityGuard.consoleGuard.proxyFilePath")}</span>
              <Input value={degradedEgressNodeFilePath} onChange={(event) => setDegradedEgressNodeFilePath(event.currentTarget.value)} />
            </label>
            <Button type="button" variant="secondary" size="sm" disabled={proxyPreviewQuery.isFetching} onClick={() => setProxyPreviewOpen(true)}>
              {t("qualityGuard.consoleGuard.proxyFilePreview")}
            </Button>
          </div>
          {!proxyFilePathValid ? <p className="text-xs text-destructive">{t("qualityGuard.consoleGuard.proxyFilePathInvalid")}</p> : null}
          <Button type="button" size="sm" disabled={!proxyFilePathValid || !proxyFilePathDirty || proxyFilePathMutation.isPending} onClick={() => proxyFilePathMutation.mutate()}>
            {t("qualityGuard.consoleGuard.proxyFileSave")}
          </Button>
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

      <Dialog open={proxyPreviewOpen} onOpenChange={setProxyPreviewOpen}>
        <DialogContent className="flex max-h-[calc(100svh-2rem)] min-h-0 flex-col gap-0 overflow-hidden p-0 sm:max-w-3xl">
          <DialogHeader className="shrink-0 px-5 py-4 pr-12">
            <DialogTitle>{t("qualityGuard.consoleGuard.proxyFilePreviewTitle")}</DialogTitle>
            <DialogDescription className="break-all">{proxyPreviewQuery.data?.path ?? savedDegradedEgressNodeFilePath}</DialogDescription>
          </DialogHeader>
          <div className="min-h-0 overflow-auto px-5 pb-5">
            {proxyPreviewQuery.isPending ? <div className="flex min-h-32 items-center justify-center"><Spinner /></div> : null}
            {proxyPreviewQuery.isError ? <p className="text-sm text-destructive">{t("qualityGuard.consoleGuard.proxyFileReadFailed")}</p> : null}
            {proxyPreviewQuery.data && !proxyPreviewQuery.isError ? <pre className="max-h-[60vh] min-h-32 overflow-auto whitespace-pre-wrap break-all rounded-md bg-muted p-3 text-xs">{proxyPreviewQuery.data.content || t("qualityGuard.consoleGuard.proxyFileEmpty")}</pre> : null}
          </div>
        </DialogContent>
      </Dialog>
    </div>
  );
}
