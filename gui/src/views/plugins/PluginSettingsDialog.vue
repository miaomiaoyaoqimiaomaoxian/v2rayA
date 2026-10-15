<script setup lang="ts">
// The settings of the plugins that need a switch of their own.
//
// Both plugins here change how traffic is fetched or routed, so neither is a
// plain on/off: the user has to know what turning it on does before they turn
// it on. That is why this is a dialog with an explanation per switch rather
// than a row of toggles on the page.
//
//   - proxyimport  fetch subscriptions through v2rayA's own proxy
//   - watchdog     keep probing each group's node so a dead one is switched
//
// The watchdog one matters more than it looks: with it off, the autogroup
// plugin still picks a node at startup but nothing notices when that node
// dies later, so failover silently does not happen. The dialog says so.
import { computed, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import type { PluginProxyImportSettings, PluginWatchdogSettings } from "@/api";

const props = defineProps<{
  proxyImport: PluginProxyImportSettings | null;
  proxyImportActive: boolean;
  watchdog: PluginWatchdogSettings | null;
  watchdogRunning: boolean;
  busy?: boolean;
}>();

const emit = defineEmits<{
  saveProxyImport: [PluginProxyImportSettings];
  saveWatchdog: [PluginWatchdogSettings];
  close: [];
}>();

const { t } = useI18n();

// Working copies: editing must not write through to the page before save.
const pi = ref<PluginProxyImportSettings>({
  preferProxy: true,
  probeDirectFirst: true,
  timeoutSeconds: 20,
});
const wd = ref<PluginWatchdogSettings>({
  intervalSeconds: 30,
  probeUrl: "",
  enabled: true,
});

watch(
  () => props.proxyImport,
  (v) => {
    if (v) pi.value = { ...v };
  },
  { immediate: true },
);
watch(
  () => props.watchdog,
  (v) => {
    if (v) wd.value = { ...v };
  },
  { immediate: true },
);

// A probe URL is optional; blank means the built-in default, so only send it
// when the user actually typed one.
const probeUrl = computed(() => wd.value.probeUrl.trim());

function saveProxyImport() {
  emit("saveProxyImport", { ...pi.value });
}

function saveWatchdog() {
  emit("saveWatchdog", { ...wd.value, probeUrl: probeUrl.value });
}
</script>

<template>
  <v-card max-width="620" class="mx-auto">
    <v-card-title>{{ t("plugins.settingsTitle") }}</v-card-title>

    <v-card-text>
      <!-- proxyimport -->
      <section class="mb-6">
        <div class="d-flex align-center ga-2 mb-1">
          <v-icon icon="mdi-import" size="20" />
          <span class="text-subtitle-1">{{
            t("plugins.proxyImportTitle")
          }}</span>
        </div>
        <p class="text-body-2 text-medium-emphasis mb-3">
          {{ t("plugins.proxyImportHint") }}
        </p>

        <v-switch
          v-model="pi.preferProxy"
          :label="t('plugins.proxyImportSwitch')"
          :hint="t('plugins.proxyImportSwitchHint')"
          persistent-hint
          density="compact"
        />
        <v-switch
          v-model="pi.probeDirectFirst"
          :label="t('plugins.proxyImportDirectFirst')"
          :hint="t('plugins.proxyImportDirectFirstHint')"
          persistent-hint
          density="compact"
        />
        <v-text-field
          v-model.number="pi.timeoutSeconds"
          type="number"
          min="5"
          :label="t('plugins.proxyImportTimeout')"
          density="compact"
          variant="outlined"
          hide-details
          class="mt-3"
        />

        <v-alert
          v-if="pi.preferProxy && !proxyImportActive"
          type="info"
          variant="tonal"
          density="compact"
          class="mt-3"
        >
          {{ t("plugins.proxyImportInactive") }}
        </v-alert>

        <v-btn
          class="mt-3"
          color="primary"
          variant="flat"
          :loading="busy"
          :disabled="!pi.preferProxy"
          @click="saveProxyImport"
        >
          {{ t("operations.save") }}
        </v-btn>
      </section>

      <v-divider class="mb-6" />

      <!-- watchdog -->
      <section>
        <div class="d-flex align-center ga-2 mb-1">
          <v-icon icon="mdi-heart-pulse" size="20" />
          <span class="text-subtitle-1">{{ t("plugins.watchdogTitle") }}</span>
        </div>
        <p class="text-body-2 text-medium-emphasis mb-3">
          {{ t("plugins.watchdogHint") }}
        </p>

        <v-alert
          v-if="!wd.enabled"
          type="warning"
          variant="tonal"
          density="compact"
          class="mb-3"
        >
          {{ t("plugins.watchdogOffWarning") }}
        </v-alert>

        <v-switch
          v-model="wd.enabled"
          :label="t('plugins.watchdogEnabled')"
          :hint="t('plugins.watchdogEnabledHint')"
          persistent-hint
          density="compact"
        />
        <v-text-field
          v-model.number="wd.intervalSeconds"
          type="number"
          min="10"
          step="5"
          :label="t('plugins.watchdogInterval')"
          :hint="t('plugins.watchdogIntervalHint')"
          persistent-hint
          density="compact"
          variant="outlined"
          class="mt-3"
        />
        <v-text-field
          v-model="wd.probeUrl"
          :label="t('plugins.watchdogProbeUrl')"
          :hint="t('plugins.watchdogProbeUrlHint')"
          persistent-hint
          density="compact"
          variant="outlined"
          class="mt-3"
        />

        <v-btn
          class="mt-4"
          color="primary"
          variant="flat"
          :loading="busy"
          @click="saveWatchdog"
        >
          {{ t("operations.save") }}
        </v-btn>
      </section>
    </v-card-text>

    <v-card-actions>
      <v-spacer />
      <v-btn variant="text" @click="emit('close')">
        {{ t("operations.close") }}
      </v-btn>
    </v-card-actions>
  </v-card>
</template>
