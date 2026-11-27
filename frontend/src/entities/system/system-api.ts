import { apiRequest } from "@/shared/api/client";
import { createObjectDecoder, hasShape, isArrayOf, isBoolean, isOneOf, isString, type ValueValidator } from "@/shared/api/decoder";

export type SystemInfoDTO = {
  publicApiBaseURL: string;
};

const decodeSystemInfo = createObjectDecoder<SystemInfoDTO>("system info", { publicApiBaseURL: isString });

export function getSystemInfo(): Promise<SystemInfoDTO> {
  return apiRequest("/api/admin/v1/system", {}, decodeSystemInfo);
}

export type UpdateStatus = "unchecked" | "up_to_date" | "update_available" | "check_failed";

export type VersionInfoDTO = {
  currentVersion: string;
  latestVersion: string;
  updateAvailable: boolean;
  status: UpdateStatus;
  checkedAt: string | null;
  releaseUrl: string;
  releaseNotes: string;
  error: string;
};

const isNullableString: ValueValidator = (value) => value === null || isString(value);
const decodeVersionInfo = createObjectDecoder<VersionInfoDTO>("version info", {
  currentVersion: isString,
  latestVersion: isString,
  updateAvailable: isBoolean,
  status: isOneOf("unchecked", "up_to_date", "update_available", "check_failed"),
  checkedAt: isNullableString,
  releaseUrl: isString,
  releaseNotes: isString,
  error: isString,
});

export function getVersionInfo(): Promise<VersionInfoDTO> {
  return apiRequest("/api/admin/v1/system/version", {}, decodeVersionInfo);
}

export function checkForUpdates(): Promise<VersionInfoDTO> {
  return apiRequest("/api/admin/v1/system/update/check", { method: "POST" }, decodeVersionInfo);
}

export type ForkReleaseDTO = {
  tag: string;
  name: string;
  url: string;
  notes: string;
  publishedAt: string;
};

const isForkRelease = hasShape({
  tag: isString,
  name: isString,
  url: isString,
  notes: isString,
  publishedAt: isString,
});

const decodeForkReleases = createObjectDecoder<{ releases: ForkReleaseDTO[] }>("fork releases", {
  releases: isArrayOf(isForkRelease),
});

export async function getForkReleases(force = false): Promise<ForkReleaseDTO[]> {
  const query = force ? "?force=1" : "";
  const data = await apiRequest(`/api/admin/v1/system/releases${query}`, {}, decodeForkReleases);
  return data.releases;
}

export type UpgradeOperationDTO = {
  id: string;
  tag: string;
  phase: string;
  message: string;
  fromVersion: string;
  requestedBy: string;
  updatedAt: string;
};

const isUpgradeOperation = hasShape({
  id: isString,
  tag: isString,
  phase: isString,
  message: isString,
  fromVersion: isString,
  requestedBy: isString,
  updatedAt: isString,
});

const isNullableOperation: ValueValidator = (value) => value === null || isUpgradeOperation(value);

export type UpgradeStatusDTO = {
  enabled: boolean;
  agentOnline: boolean;
  agentUpdatedAt: string;
  imageRepo: string;
  currentTag: string;
  currentVersion: string;
  inProgress: boolean;
  operation: UpgradeOperationDTO | null;
};

const decodeUpgradeStatus = createObjectDecoder<UpgradeStatusDTO>("upgrade status", {
  enabled: isBoolean,
  agentOnline: isBoolean,
  agentUpdatedAt: isString,
  imageRepo: isString,
  currentTag: isString,
  currentVersion: isString,
  inProgress: isBoolean,
  operation: isNullableOperation,
});

const decodeUpgradeOperation = createObjectDecoder<UpgradeOperationDTO>("upgrade operation", {
  id: isString,
  tag: isString,
  phase: isString,
  message: isString,
  fromVersion: isString,
  requestedBy: isString,
  updatedAt: isString,
});

export function getUpgradeStatus(): Promise<UpgradeStatusDTO> {
  return apiRequest("/api/admin/v1/system/upgrade/status", {}, decodeUpgradeStatus);
}

export function requestUpgrade(tag: string): Promise<UpgradeOperationDTO> {
  return apiRequest("/api/admin/v1/system/upgrade", { method: "POST", body: { tag } }, decodeUpgradeOperation);
}
