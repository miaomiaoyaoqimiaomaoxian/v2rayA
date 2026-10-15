// The plugins page model: what is installed, what is running, and the
// per-group automation policy of the autogroup plugin.
//
// The page shows a condensed card per plugin; the detail lives in a dialog.
// That split keeps the list scannable when many plugins are installed and
// matches how the subscriptions page treats its cards.
import { ref } from "vue";
import {
  deletePlugin,
  getPluginGroups,
  getPluginLog,
  getPluginProxyImport,
  getPlugins,
  getPluginWatchdog,
  putPlugin,
  putPluginLog,
  putPluginPolicy,
  putPluginProxyImport,
  putPluginWatchdog,
  postPluginTest,
} from "@/api";
import type {
  PluginGroupStatus,
  PluginInfo,
  PluginProxyImportSettings,
  PluginWatchdogSettings,
} from "@/api";
import { errorText } from "@/api/errors";

// 组件从 model 取类型，不必再从 @/api 引一次。
export type { PluginGroupStatus };

/** 一行插件：后端字段 + 该插件的分组视图。 */
export interface PluginRow extends PluginInfo {
  /** 仅 autogroup 有分组；其它插件为空数组 */
  groups: PluginGroupStatus[];
}

export interface PluginLogState {
  level: string;
  path: string;
  levels: string[];
  running?: boolean;
}

export function usePlugins() {
  const plugins = ref<PluginRow[]>([]);
  const loading = ref(true);
  const loadError = ref("");
  /** the row whose detail dialog is open; null means none */
  const detail = ref<PluginRow | null>(null);
  /** the row whose policy dialog is open; null means none */
  const editing = ref<PluginGroupStatus | null>(null);
  const testing = ref("");
  const busy = ref(false);
  const logState = ref<PluginLogState>({ level: "info", path: "", levels: [] });

  /**
   * Groups the watchdog could not route at all. The user asked for a hint when
   * a node dies for good; a group that has no working node is the case worth
   * interrupting for, because nothing else in the UI shows it.
   */
  const alerts = ref<Record<string, string>>({});
  /** proxyimport: fetch subscriptions through v2rayA. */
  const proxyImport = ref<PluginProxyImportSettings | null>(null);
  const proxyImportActive = ref(false);
  /** autogroup's watchdog, i.e. whether failover actually runs. */
  const watchdog = ref<PluginWatchdogSettings | null>(null);
  const watchdogRunning = ref(false);

  /** 把分组状态挂到 autogroup 上；其它插件没有分组概念。 */
  function withGroups(
    list: PluginInfo[],
    groups: PluginGroupStatus[],
  ): PluginRow[] {
    const byOutbound = new Map(groups.map((g) => [g.outbound, g]));
    return list.map((p) => ({
      ...p,
      groups: p.name === "autogroup" ? [...byOutbound.values()] : [],
    }));
  }

  async function sync() {
    loading.value = true;
    loadError.value = "";
    try {
      const [list, groupResp, log, pi, wd] = await Promise.all([
        getPlugins().then((r) => r.plugins),
        // A group endpoint failure must not blank the whole page: the plugin
        // list is still useful without it.
        getPluginGroups().catch(() => null),
        getPluginLog().catch(() => null),
        getPluginProxyImport().catch(() => null),
        getPluginWatchdog().catch(() => null),
      ]);
      const groups = groupResp?.groups ?? [];
      alerts.value = groupResp?.alerts ?? {};
      plugins.value = withGroups(
        list,
        groups.map((g) => g.status),
      );
      if (log) logState.value = log;
      if (pi) {
        proxyImport.value = pi.settings;
        proxyImportActive.value = pi.active;
      }
      if (wd) {
        watchdog.value = wd.settings;
        watchdogRunning.value = wd.running;
      }
    } catch (err) {
      loadError.value = errorText(err);
      throw err;
    } finally {
      loading.value = false;
    }
  }

  async function setEnabled(name: string, enabled: boolean) {
    busy.value = true;
    try {
      const [{ plugins: list }, groupResp, pi, wd] = await Promise.all([
        putPlugin({ name, enabled }),
        getPluginGroups().catch(() => null),
        getPluginProxyImport().catch(() => null),
        getPluginWatchdog().catch(() => null),
      ]);
      alerts.value = groupResp?.alerts ?? {};
      plugins.value = withGroups(
        list,
        (groupResp?.groups ?? []).map((g) => g.status),
      );
      if (pi) {
        proxyImport.value = pi.settings;
        proxyImportActive.value = pi.active;
      }
      if (wd) {
        watchdog.value = wd.settings;
        watchdogRunning.value = wd.running;
      }
      // 详情对话框开着时同步它，否则会显示旧状态。
      if (detail.value?.name === name)
        detail.value = plugins.value.find((p) => p.name === name) ?? null;
    } finally {
      busy.value = false;
    }
  }

  /** Store the proxyimport switch; `active` tells us if it took effect. */
  async function saveProxyImport(settings: PluginProxyImportSettings) {
    busy.value = true;
    try {
      const res = await putPluginProxyImport(settings);
      proxyImport.value = res.settings;
      proxyImportActive.value = res.active;
    } finally {
      busy.value = false;
    }
  }

  /** Store the watchdog settings; they take effect without a restart. */
  async function saveWatchdog(settings: PluginWatchdogSettings) {
    busy.value = true;
    try {
      const res = await putPluginWatchdog(settings);
      watchdog.value = res.settings;
      watchdogRunning.value = res.running;
    } finally {
      busy.value = false;
    }
  }

  async function uninstall(name: string) {
    busy.value = true;
    try {
      const { plugins: list } = await deletePlugin({ name });
      plugins.value = list.map((p) => ({ ...p, groups: [] }));
      detail.value = null;
    } finally {
      busy.value = false;
    }
  }

  async function savePolicy(
    outbound: string,
    policy: {
      mode: string;
      failThreshold: number;
      maxSwitches: number;
      autoTest: boolean;
    },
  ) {
    busy.value = true;
    try {
      await putPluginPolicy({ outbound, policy });
      editing.value = null;
      await sync();
    } finally {
      busy.value = false;
    }
  }

  async function setLogLevel(level: string) {
    busy.value = true;
    try {
      const { level: cur } = await putPluginLog({ level });
      logState.value = { ...logState.value, level: cur };
    } finally {
      busy.value = false;
    }
  }

  /** Re-test every node of one group and refresh what the plugin saw. */
  async function testGroup(outbound: string) {
    testing.value = outbound;
    try {
      await postPluginTest({ outbound });
      await sync();
    } finally {
      testing.value = "";
    }
  }

  return {
    plugins,
    loading,
    loadError,
    detail,
    editing,
    testing,
    busy,
    logState,
    alerts,
    proxyImport,
    proxyImportActive,
    watchdog,
    watchdogRunning,
    sync,
    setEnabled,
    uninstall,
    savePolicy,
    saveProxyImport,
    saveWatchdog,
    setLogLevel,
    testGroup,
  };
}
