// The plugins page model: what is installed, what is running, and the
// per-group automation policy of the autogroup plugin.
//
// The page shows a condensed card per plugin; the detail lives in a dialog.
// That split keeps the list scannable when many plugins are installed and
// matches how the subscriptions page treats its cards.
import { ref } from "vue";
import {
  deletePlugin,
  getPluginGroupStatuses,
  getPluginLog,
  getPlugins,
  putPlugin,
  putPluginLog,
  putPluginPolicy,
  postPluginTest,
} from "@/api";
import type { PluginGroupStatus, PluginInfo } from "@/api";
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
      const [list, groups, log] = await Promise.all([
        getPlugins().then((r) => r.plugins),
        getPluginGroupStatuses().catch(() => [] as PluginGroupStatus[]),
        getPluginLog().catch(() => null),
      ]);
      plugins.value = withGroups(list, groups);
      if (log) logState.value = log;
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
      const [{ plugins: list }, groups] = await Promise.all([
        putPlugin({ name, enabled }),
        getPluginGroupStatuses().catch(() => [] as PluginGroupStatus[]),
      ]);
      plugins.value = withGroups(list, groups);
      // 详情对话框开着时同步它，否则会显示旧状态。
      if (detail.value?.name === name)
        detail.value = plugins.value.find((p) => p.name === name) ?? null;
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
    sync,
    setEnabled,
    uninstall,
    savePolicy,
    setLogLevel,
    testGroup,
  };
}
