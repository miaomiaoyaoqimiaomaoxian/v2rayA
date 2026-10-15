<script setup lang="ts">
// One plugin as a condensed card, in the shape of the subscriptions page's
// cards: name on top, facts below, and the controls the user reaches for most
// (the on/off switch) in the top-right corner. Everything else lives behind a
// click that opens the detail dialog.
import { computed } from "vue";
import { useI18n } from "vue-i18n";
import { mdiChevronRight, mdiPuzzle } from "@mdi/js";
import type { PluginRow } from "./model";

defineOptions({ name: "PluginCard" });
const props = defineProps<{
  plugin: PluginRow;
  disabled: boolean;
  testing: boolean;
}>();
const emit = defineEmits<{
  (e: "open", plugin: PluginRow): void;
  (e: "toggle", name: string, enabled: boolean): void;
  (e: "test", outbound: string): void;
}>();

const { t } = useI18n();

const enabled = computed(() => props.plugin.state === "enabled");
const statusText = computed(() =>
  props.plugin.running ? t("plugins.stateRunning") : t("plugins.stateStopped"),
);
// “日期”取决策日志里最近一次事件的���间；没有事件时只显示版本。
const lastEvent = computed(() => {
  // groups 里没有时间信息，用插件自身的时间戳（后端补齐前先退化为版本号）
  return props.plugin.version;
});
</script>

<template>
  <v-card
    color="surface-container-high"
    variant="flat"
    rounded="xl"
    class="plugin-card"
    :loading="disabled"
    role="button"
    tabindex="0"
    @click="emit('open', plugin)"
    @keydown.enter="emit('open', plugin)"
    @keydown.space.prevent="emit('open', plugin)"
  >
    <v-card-item class="pa-4">
      <template #prepend>
        <v-avatar
          color="primary"
          variant="tonal"
          rounded="lg"
          size="40"
          class="flex-shrink-0"
        >
          <v-icon :icon="mdiPuzzle" size="20" />
        </v-avatar>
      </template>

      <v-card-title class="md3-title-medium text-wrap" dir="auto">
        {{ plugin.name }}
      </v-card-title>

      <template #append>
        <!-- 右上角：启用/关闭。点卡片本身不该切换，所以阻止冒泡。 -->
        <v-switch
          :model-value="enabled"
          color="primary"
          density="compact"
          hide-details
          class="plugin-card__switch"
          :disabled="disabled"
          @click.stop
          @update:model-value="emit('toggle', plugin.name, $event === true)"
        />
        <v-icon
          :icon="mdiChevronRight"
          size="20"
          color="on-surface-variant"
          class="flex-shrink-0"
        />
      </template>
    </v-card-item>

    <v-card-text class="px-4 pb-4 pt-0">
      <p
        class="md3-body-medium text-on-surface-variant mb-2 text-wrap"
        dir="auto"
      >
        {{ plugin.description }}
      </p>
      <div class="d-flex flex-wrap ga-2">
        <v-chip
          size="small"
          variant="tonal"
          :color="plugin.running ? 'success' : undefined"
        >
          {{ statusText }}
        </v-chip>
        <v-chip size="small" variant="tonal">
          {{ t("plugins.groupsCount") }}: {{ plugin.groups.length }}
        </v-chip>
        <v-chip v-if="plugin.builtin" size="small" variant="outlined">
          {{ t("plugins.builtin") }}
        </v-chip>
        <v-chip size="small" variant="outlined" dir="ltr">
          {{ plugin.name }} {{ lastEvent }}
        </v-chip>
      </div>

      <v-btn
        v-if="plugin.groups.length"
        variant="text"
        density="compact"
        size="small"
        class="mt-2 px-0"
        :loading="testing"
        :disabled="disabled"
        @click.stop="emit('test', plugin.groups[0].outbound)"
      >
        {{ t("plugins.testNodes") }}
      </v-btn>
    </v-card-text>
  </v-card>
</template>

<style scoped>
.plugin-card {
  overflow-wrap: anywhere;
  cursor: pointer;
  transition: background-color 120ms ease;
}
.plugin-card:hover {
  background-color: rgb(var(--v-theme-surface-container-highest));
}
.plugin-card:focus-visible {
  outline: 2px solid rgb(var(--v-theme-primary));
  outline-offset: 2px;
}
.plugin-card__switch {
  margin-right: 4px;
}
</style>
