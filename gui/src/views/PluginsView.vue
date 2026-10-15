<script setup lang="ts">
// The plugins page: a condensed card per plugin, plus a management dialog.
//
// The list stays scannable — name, description, state and a switch in the top
// right — and everything else lives behind a click on the card. That mirrors
// how the subscriptions page treats its cards.
import { computed, onMounted, ref } from "vue";
import { useI18n } from "vue-i18n";
import { useDisplay } from "vuetify";
import { mdiCogOutline, mdiPuzzle, mdiRefresh } from "@mdi/js";
import { useConfirm, useNotify } from "@/composables";
import { errorText } from "@/api/errors";
import type { PluginRow } from "./plugins/model";
import { usePlugins } from "./plugins/model";
import PluginCard from "./plugins/PluginCard.vue";
import PluginDetailDialog from "./plugins/PluginDetailDialog.vue";
import PluginManageDialog from "./plugins/PluginManageDialog.vue";
import PluginPolicyDialog from "./plugins/PluginPolicyDialog.vue";
import PluginSettingsDialog from "./plugins/PluginSettingsDialog.vue";

defineOptions({ name: "PluginsView" });
const { t } = useI18n();
const { width } = useDisplay();
const expanded = computed(() => width.value >= 840);
const notify = useNotify();
const askConfirm = useConfirm();
const model = usePlugins();
const {
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
} = model;
const disabled = computed(() => busy.value || loading.value);

const managing = ref(false);
const settingsOpen = ref(false);
/** 分组名 -> 原因。空表示没有分组彻底不可用。 */
const alertEntries = computed(() => Object.entries(alerts.value ?? {}));

onMounted(sync);
defineExpose({ sync });

async function toggle(name: string, enabled: boolean) {
  try {
    await model.setEnabled(name, enabled);
    notify.success(enabled ? t("plugins.enabled") : t("plugins.disabled"));
  } catch (err) {
    notify.warning(errorText(err));
    // 切换失败时把界面拨回去，否则开关会停在一个没生效的位置。
    await sync().catch(() => {});
  }
}

async function uninstall(plugin: PluginRow) {
  if (plugin.builtin) {
    notify.info(t("plugins.builtinHint"));
    return;
  }
  const ok = await askConfirm({
    title: t("plugins.uninstallTitle"),
    message: t("plugins.uninstallMessage", { name: plugin.name }),
    confirmText: t("operations.delete"),
    cancelText: t("operations.cancel"),
    destructive: true,
  });
  if (!ok) return;
  try {
    await model.uninstall(plugin.name);
    notify.success(t("plugins.uninstalled"));
  } catch (err) {
    notify.warning(errorText(err));
  }
}

async function testGroup(outbound: string) {
  try {
    await model.testGroup(outbound);
    notify.success(t("plugins.tested"));
  } catch (err) {
    notify.warning(errorText(err));
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
  try {
    await model.savePolicy(outbound, policy);
    notify.success(t("plugins.policySaved"));
  } catch (err) {
    notify.warning(errorText(err));
  }
}

async function setLevel(level: string) {
  try {
    await model.setLogLevel(level);
  } catch (err) {
    notify.warning(errorText(err));
  }
}

async function saveProxyImport(
  settings: Parameters<typeof model.saveProxyImport>[0],
) {
  try {
    await model.saveProxyImport(settings);
    notify.success(t("plugins.policySaved"));
  } catch (err) {
    notify.warning(errorText(err));
  }
}

async function saveWatchdog(
  settings: Parameters<typeof model.saveWatchdog>[0],
) {
  try {
    await model.saveWatchdog(settings);
    notify.success(t("plugins.policySaved"));
    // The watchdog loop may have started or stopped; refresh so the banner
    // and the switch state match reality.
    await sync().catch(() => {});
  } catch (err) {
    notify.warning(errorText(err));
  }
}

function uninstallByName(name: string) {
  const p = plugins.value.find((x) => x.name === name);
  if (p) void uninstall(p);
}

// 添加/导入/导出目前只覆盖内置插件的现状：外部插件通道尚未接通。
// 先如实告诉用户，不做假动作。
function notYet(action: string) {
  notify.info(t("plugins.notYet", { action }));
}
</script>

<template>
  <div class="plugins" :class="{ 'plugins--expanded': expanded }">
    <div class="d-flex flex-wrap align-center ga-2 mb-4">
      <span class="text-on-surface-variant">{{ t("plugins.intro") }}</span>
      <v-spacer />
      <v-btn
        variant="text"
        :prepend-icon="mdiRefresh"
        :disabled="disabled"
        @click="sync"
        >{{ t("operations.update") }}</v-btn
      >
      <v-btn
        variant="flat"
        color="primary"
        :prepend-icon="mdiCogOutline"
        :disabled="disabled"
        @click="managing = true"
        >{{ t("plugins.manage") }}</v-btn
      >
      <v-btn
        variant="text"
        :prepend-icon="mdiCogOutline"
        :disabled="disabled"
        @click="settingsOpen = true"
        >{{ t("plugins.settings") }}</v-btn
      >
    </div>

    <v-alert v-if="loadError" type="error" variant="tonal" class="mb-4">
      {{ loadError }}
      <template #append
        ><v-btn variant="text" @click="sync">{{
          t("operations.update")
        }}</v-btn></template
      >
    </v-alert>

    <!--
      A group whose every candidate is unreachable is the case worth
      interrupting for: nothing else on the page shows it, and the user asked
      to be told when a node dies for good rather than discovering it as a
      silent loss of access.
    -->
    <v-alert
      v-if="alertEntries.length"
      type="error"
      variant="tonal"
      class="mb-4"
    >
      <div class="text-subtitle-2 mb-1">
        {{ t("plugins.allNodesDown", { count: alertEntries.length }) }}
      </div>
      <div v-for="[group, reason] in alertEntries" :key="group">
        {{ group }} — {{ reason }}
      </div>
    </v-alert>

    <!--
      With the watchdog off the plugin still picks a node at startup, but
      nothing notices when that node dies later, so failover silently does not
      happen. Say so rather than letting the user assume it works.
    -->
    <v-alert
      v-if="watchdog && !watchdog.enabled"
      type="warning"
      variant="tonal"
      density="compact"
      class="mb-4"
    >
      {{ t("plugins.watchdogOffWarning") }}
    </v-alert>

    <v-skeleton-loader
      v-if="loading && !plugins.length"
      type="card, card"
      class="bg-transparent"
    />

    <div v-else-if="plugins.length" class="plugins__cards">
      <PluginCard
        v-for="p in plugins"
        :key="p.name"
        :plugin="p"
        :disabled="disabled"
        :testing="testing === (p.groups[0]?.outbound ?? '')"
        @open="detail = $event"
        @toggle="toggle"
        @test="testGroup"
      />
    </div>

    <v-sheet
      v-else-if="!loadError"
      color="surface-container-low"
      rounded="xl"
      class="plugins__empty"
    >
      <v-icon :icon="mdiPuzzle" size="28" color="on-surface-variant" />
      <div class="plugins__empty-text">
        <p class="md3-title-medium ma-0">{{ t("plugins.none") }}</p>
        <p class="md3-body-medium text-on-surface-variant ma-0">
          {{ t("plugins.noneHint") }}
        </p>
      </div>
      <v-btn
        variant="tonal"
        color="primary"
        class="flex-shrink-0"
        :disabled="disabled"
        @click="managing = true"
        >{{ t("plugins.manage") }}</v-btn
      >
    </v-sheet>

    <PluginDetailDialog
      v-if="detail"
      :plugin="detail"
      :busy="busy"
      :testing="testing"
      :log-level="logState.level"
      :log-path="logState.path"
      :log-levels="logState.levels"
      @close="detail = null"
      @toggle="toggle"
      @uninstall="detail && uninstall(detail)"
      @test="testGroup"
      @set-level="setLevel"
      @save-policy="savePolicy"
    />

    <PluginManageDialog
      v-if="managing"
      :plugins="plugins"
      :busy="busy"
      @close="managing = false"
      @add="notYet(t('plugins.add'))"
      @uninstall="uninstallByName"
      @import="notYet(t('plugins.import'))"
      @export="notYet(t('plugins.export'))"
    />

    <PluginPolicyDialog
      v-if="editing"
      :group="editing"
      :busy="busy"
      @close="editing = null"
      @save="model.savePolicy(editing.outbound, $event)"
    />

    <PluginSettingsDialog
      v-if="settingsOpen"
      :proxy-import="proxyImport"
      :proxy-import-active="proxyImportActive"
      :watchdog="watchdog"
      :watchdog-running="watchdogRunning"
      :busy="busy"
      @close="settingsOpen = false"
      @save-proxy-import="saveProxyImport"
      @save-watchdog="saveWatchdog"
    />
  </div>
</template>

<style scoped>
.plugins__cards {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: 16px;
}
.plugins--expanded .plugins__cards {
  grid-template-columns: repeat(auto-fill, minmax(380px, 1fr));
}
.plugins__empty {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 20px 24px;
}
.plugins__empty-text {
  flex: 1;
}
</style>
