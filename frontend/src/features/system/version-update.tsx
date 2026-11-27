import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowUpRight, CheckCircle2, Info, RefreshCw, RotateCcw, Upload, XCircle } from "lucide-react";
import { type ReactNode, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { checkForUpdates, getForkReleases, getUpgradeStatus, getVersionInfo, requestUpgrade } from "@/entities/system/system-api";
import { cn } from "@/shared/lib/cn";
import { formatDateTime } from "@/shared/lib/format";

const versionQueryKey = ["system-version"] as const;
const releasesQueryKey = ["system-releases"] as const;
const upgradeStatusQueryKey = ["system-upgrade-status"] as const;

function useVersionInfo() {
  return useQuery({
    queryKey: versionQueryKey,
    queryFn: getVersionInfo,
    staleTime: 60_000,
    retry: 1,
  });
}

function useCheckForUpdates() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: checkForUpdates,
    onSuccess: (value) => {
      queryClient.setQueryData(versionQueryKey, value);
      void queryClient.invalidateQueries({ queryKey: releasesQueryKey });
    },
  });
}

function useForkReleases() {
  return useQuery({
    queryKey: releasesQueryKey,
    queryFn: () => getForkReleases(false),
    staleTime: 60_000,
    retry: 1,
  });
}

function useUpgradeStatus(trackingOpId: string | null) {
  return useQuery({
    queryKey: upgradeStatusQueryKey,
    queryFn: getUpgradeStatus,
    staleTime: trackingOpId ? 0 : 15_000,
    refetchInterval: (query) => {
      const data = query.state.data;
      if (trackingOpId) {
        const op = data?.operation;
        if (op && op.id === trackingOpId && (op.phase === "succeeded" || op.phase === "failed")) {
          return false;
        }
        return 2000;
      }
      return data?.inProgress ? 2000 : false;
    },
    retry: (failureCount) => (trackingOpId ? false : failureCount < 1),
  });
}

export function CurrentVersionLabel() {
  const versionQuery = useVersionInfo();
  const version = versionQuery.data?.currentVersion;
  if (!version) return null;
  return <span className="font-mono text-[10px] font-normal text-muted-foreground">{version}</span>;
}

export function VersionUpdateBanner() {
  const { t } = useTranslation();
  const versionQuery = useVersionInfo();
  const checkMutation = useCheckForUpdates();
  const version = versionQuery.data;
  if (!version?.updateAvailable) return null;

  return (
    <section className="flex flex-col gap-3 rounded-lg bg-amber-500/10 px-4 py-3 sm:flex-row sm:items-center sm:justify-between">
      <div className="min-w-0">
        <p className="text-sm font-medium">{t("updates.available", { version: version.latestVersion })}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{t("updates.currentSummary", { version: version.currentVersion })}</p>
      </div>
      <div className="flex shrink-0 items-center gap-0.5">
        {version.releaseUrl ? (
          <Button variant="ghost" size="sm" className="h-7 px-2.5 text-xs font-normal text-muted-foreground hover:text-foreground" asChild>
            <a href={version.releaseUrl} target="_blank" rel="noreferrer">{t("updates.viewRelease")}<ArrowUpRight className="size-3.5" /></a>
          </Button>
        ) : null}
        {version.releaseUrl ? <span className="mx-1 h-3 w-px bg-border/70" /> : null}
        <Button variant="ghost" size="sm" className="h-7 px-2.5 text-xs font-normal text-muted-foreground hover:text-foreground" disabled={checkMutation.isPending} onClick={() => checkMutation.mutate()}>
          {checkMutation.isPending ? <Spinner /> : <RefreshCw className="size-3.5" />}{t("updates.checkNow")}
        </Button>
      </div>
    </section>
  );
}

export function VersionUpdateSection() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const versionQuery = useVersionInfo();
  const checkMutation = useCheckForUpdates();
  const releasesQuery = useForkReleases();

  const [selectedTag, setSelectedTag] = useState("");
  const [confirmTag, setConfirmTag] = useState<string | null>(null);
  const [trackingOpId, setTrackingOpId] = useState<string | null>(null);
  const completedOpRef = useRef<string | null>(null);

  const statusQuery = useUpgradeStatus(trackingOpId);

  const upgradeMutation = useMutation({
    mutationFn: requestUpgrade,
    onSuccess: (op) => {
      setTrackingOpId(op.id);
      void queryClient.invalidateQueries({ queryKey: upgradeStatusQueryKey });
    },
  });

  const refreshReleasesMutation = useMutation({
    mutationFn: () => getForkReleases(true),
    onSuccess: (list) => queryClient.setQueryData(releasesQueryKey, list),
  });

  const version = versionQuery.data;
  const releases = releasesQuery.data ?? [];
  const upgradeStatus = statusQuery.data;
  const activeOp = upgradeStatus?.operation ?? null;

  const trackedTerminal = Boolean(
    trackingOpId &&
      activeOp &&
      activeOp.id === trackingOpId &&
      (activeOp.phase === "succeeded" || activeOp.phase === "failed"),
  );
  const isPolling = Boolean((trackingOpId && !trackedTerminal) || upgradeStatus?.inProgress);

  useEffect(() => {
    if (!trackingOpId || !trackedTerminal || !activeOp) return;
    if (completedOpRef.current === activeOp.id) return;
    completedOpRef.current = activeOp.id;
    void queryClient.invalidateQueries({ queryKey: versionQueryKey });
    void queryClient.invalidateQueries({ queryKey: releasesQueryKey });
    if (activeOp.phase === "succeeded") {
      checkMutation.mutate();
    }
  }, [activeOp, checkMutation, queryClient, trackedTerminal, trackingOpId]);

  const effectiveSelectedTag = selectedTag || releases[0]?.tag || "";
  const currentVersion = version?.currentVersion || upgradeStatus?.currentVersion || "";
  const latestForkTag = releases[0]?.tag || "";
  const canUpgradeToLatest = Boolean(
    upgradeStatus?.agentOnline &&
      !isPolling &&
      latestForkTag &&
      latestForkTag !== currentVersion,
  );
  const canSwitchSelected = Boolean(
    upgradeStatus?.agentOnline &&
      !isPolling &&
      effectiveSelectedTag &&
      effectiveSelectedTag !== currentVersion,
  );

  const requestError = versionQuery.error instanceof Error ? versionQuery.error.message : "";
  const checkError = checkMutation.error instanceof Error ? checkMutation.error.message : "";
  const error = version?.error || checkError || requestError;
  const upgradeError = upgradeMutation.error instanceof Error ? upgradeMutation.error.message : "";
  const reconnecting = Boolean(trackingOpId && !trackedTerminal) && statusQuery.isError;

  const phaseLabel = (phase: string) => {
    const key = `updates.phases.${phase}`;
    const translated = t(key);
    return translated === key ? phase : translated;
  };

  return (
    <div className="w-full space-y-8">
      <section className="space-y-3">
        <div className="flex min-h-8 items-center px-1">
          <h2 className="text-sm font-medium tracking-tight">{t("updates.title")}</h2>
        </div>
        <div className="flex items-start gap-3 rounded-md bg-amber-500/10 px-4 py-3">
          <Info className="mt-0.5 size-4 shrink-0 text-amber-700 dark:text-amber-300" />
          <div className="min-w-0">
            <p className="text-xs font-medium">{t("updates.noteTitle")}</p>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("updates.noteDescription")}</p>
          </div>
        </div>
        <div className="space-y-0">
          <VersionField label={t("updates.currentVersion")} description={t("updates.currentVersionHelp")}>
            <div className="flex min-w-0 items-center gap-2">
              <div className="min-w-0 flex-1"><VersionValue>{version?.currentVersion || "-"}</VersionValue></div>
              <Button type="button" variant="secondary" size="sm" className="shrink-0" disabled={versionQuery.isPending || checkMutation.isPending} onClick={() => checkMutation.mutate()}>
                {versionQuery.isPending || checkMutation.isPending ? <Spinner /> : <RefreshCw />}{t("updates.checkNow")}
              </Button>
            </div>
          </VersionField>
          <VersionField label={t("updates.latestVersion")} description={t("updates.latestVersionHelp")}>
            <VersionValue>{version?.latestVersion || t("updates.notChecked")}</VersionValue>
          </VersionField>
          <VersionField label={t("updates.statusLabel")} description={t("updates.statusLabelHelp")}>
            <VersionValue>
              {version?.status ? <span className={cn("size-1.5 shrink-0 rounded-full bg-muted-foreground", version.status === "up_to_date" && "bg-emerald-500", version.status === "update_available" && "bg-amber-500", version.status === "check_failed" && "bg-destructive")} /> : null}
              <span>{version ? t(`updates.status.${version.status}`) : t("common.loading")}</span>
            </VersionValue>
          </VersionField>
          <VersionField label={t("updates.checkedAt")} description={t("updates.checkedAtHelp")}>
            <VersionValue>{version?.checkedAt ? formatDateTime(version.checkedAt, i18n.language) : t("updates.neverChecked")}</VersionValue>
          </VersionField>
        </div>
        {error ? <p className="text-xs leading-5 text-destructive">{error}</p> : null}
      </section>

      <section className="space-y-3">
        <div className="flex min-h-8 items-center justify-between gap-3 px-1">
          <div>
            <h3 className="text-sm font-medium tracking-tight">{t("updates.upgradeTitle")}</h3>
            <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("updates.upgradeDescription")}</p>
          </div>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            disabled={refreshReleasesMutation.isPending}
            onClick={() => refreshReleasesMutation.mutate()}
          >
            {refreshReleasesMutation.isPending ? <Spinner /> : <RefreshCw />}
            {t("common.refresh")}
          </Button>
        </div>

        <div className="space-y-0">
          <VersionField label={t("updates.agentStatusLabel")} description={t("updates.agentStatusHelp")}>
            <VersionValue>
              <span
                className={cn(
                  "size-1.5 shrink-0 rounded-full",
                  upgradeStatus?.agentOnline ? "bg-emerald-500" : "bg-muted-foreground",
                )}
              />
              <span>
                {!upgradeStatus
                  ? t("common.loading")
                  : upgradeStatus.agentOnline
                    ? t("updates.agentOnline")
                    : upgradeStatus.enabled
                      ? t("updates.agentOffline")
                      : t("updates.agentDisabled")}
              </span>
            </VersionValue>
          </VersionField>

          <VersionField label={t("updates.targetVersionLabel")} description={t("updates.targetVersionHelp")}>
            <div className="flex min-w-0 flex-wrap items-center gap-2">
              <div className="min-w-[170px] flex-1">
                <Select
                  value={effectiveSelectedTag}
                  onValueChange={setSelectedTag}
                  disabled={releases.length === 0 || isPolling}
                >
                  <SelectTrigger className="h-8 text-xs">
                    <SelectValue placeholder={t("updates.noPublishedReleases")} />
                  </SelectTrigger>
                  <SelectContent>
                    {releases.map((item) => (
                      <SelectItem key={item.tag} value={item.tag} className="text-xs">
                        {item.tag}{item.tag === currentVersion ? ` (${t("updates.currentTagBadge")})` : ""}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
              {canUpgradeToLatest && latestForkTag === effectiveSelectedTag ? (
                <Button
                  type="button"
                  size="sm"
                  className="shrink-0"
                  disabled={upgradeMutation.isPending}
                  onClick={() => setConfirmTag(latestForkTag)}
                >
                  {upgradeMutation.isPending ? <Spinner /> : <Upload />}
                  {t("updates.upgradeNow")}
                </Button>
              ) : (
                <Button
                  type="button"
                  variant="secondary"
                  size="sm"
                  className="shrink-0"
                  disabled={!canSwitchSelected || upgradeMutation.isPending}
                  onClick={() => setConfirmTag(effectiveSelectedTag)}
                >
                  {upgradeMutation.isPending ? <Spinner /> : <RotateCcw />}
                  {t("updates.switchVersion")}
                </Button>
              )}
            </div>
          </VersionField>
        </div>

        {reconnecting ? (
          <div className="flex items-center gap-2.5 rounded-md bg-amber-500/10 px-4 py-3 text-xs text-amber-800 dark:text-amber-200">
            <Spinner />
            <span>{t("updates.reconnecting")}</span>
          </div>
        ) : activeOp ? (
          <div
            className={cn(
              "flex items-start gap-2.5 rounded-md px-4 py-3 text-xs",
              activeOp.phase === "succeeded" && "bg-emerald-500/10 text-emerald-800 dark:text-emerald-200",
              activeOp.phase === "failed" && "bg-destructive/10 text-destructive",
              activeOp.phase !== "succeeded" && activeOp.phase !== "failed" && "bg-secondary/60 text-foreground",
            )}
          >
            {activeOp.phase === "succeeded" ? (
              <CheckCircle2 className="mt-0.5 size-4 shrink-0" />
            ) : activeOp.phase === "failed" ? (
              <XCircle className="mt-0.5 size-4 shrink-0" />
            ) : (
              <Spinner />
            )}
            <div className="min-w-0 space-y-0.5">
              <p className="font-medium">
                {activeOp.tag} · {phaseLabel(activeOp.phase)}
              </p>
              {activeOp.message ? <p className="break-words text-muted-foreground">{activeOp.message}</p> : null}
              {activeOp.updatedAt ? (
                <p className="text-[11px] text-muted-foreground">
                  {formatDateTime(activeOp.updatedAt, i18n.language)}
                </p>
              ) : null}
            </div>
          </div>
        ) : null}

        {upgradeError ? <p className="text-xs leading-5 text-destructive">{upgradeError}</p> : null}
      </section>

      {version?.releaseNotes || version?.releaseUrl ? (
        <section className="space-y-3">
          <div className="flex min-h-8 items-center justify-between gap-3 px-1">
            <div>
              <h3 className="text-sm font-medium tracking-tight">{t("updates.releaseNotes")}</h3>
              <p className="mt-1 text-xs leading-5 text-muted-foreground">{t("updates.releaseNotesHelp")}</p>
            </div>
            {version.releaseUrl ? (
              <Button type="button" variant="secondary" size="sm" asChild>
                <a href={version.releaseUrl} target="_blank" rel="noreferrer">{t("updates.openRelease")}<ArrowUpRight /></a>
              </Button>
            ) : null}
          </div>
          <div className="min-w-0 rounded-md bg-secondary/35 px-4 py-3">
            <p className="whitespace-pre-wrap break-words text-xs leading-5 text-muted-foreground">
              {version.releaseNotes || t("updates.noReleaseNotes")}
            </p>
          </div>
        </section>
      ) : null}

      <AlertDialog open={Boolean(confirmTag)} onOpenChange={(open) => { if (!open) setConfirmTag(null); }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{t("updates.confirmTitle", { version: confirmTag ?? "" })}</AlertDialogTitle>
            <AlertDialogDescription>
              {t("updates.confirmDescription", { from: currentVersion || "-", to: confirmTag ?? "" })}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("common.cancel")}</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (!confirmTag) return;
                const tag = confirmTag;
                setConfirmTag(null);
                upgradeMutation.mutate(tag);
              }}
            >
              {t("updates.confirmAction")}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function VersionField({ label, description, children }: { label: string; description: string; children: ReactNode }) {
  return (
    <div className="min-w-0 py-4">
      <div className="grid min-w-0 gap-2.5 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)] sm:items-center sm:gap-8">
        <div className="min-w-0">
          <p className="text-xs font-medium">{label}</p>
          <p className="mt-1 max-w-xl text-xs leading-5 text-muted-foreground">{description}</p>
        </div>
        <div className="min-w-0">{children}</div>
      </div>
    </div>
  );
}

function VersionValue({ children }: { children: ReactNode }) {
  return <div className="flex min-h-8 min-w-0 items-center gap-2 rounded-md bg-secondary/55 px-3 py-1 text-xs font-medium">{children}</div>;
}
