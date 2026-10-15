<script setup lang="ts">
// Two signs that something runs, mounted once inside v-app by the shell:
// a linear progress bar along the top while any request is in flight
// (shown only once it has taken a moment, so quick calls do not flash),
// and the full-page overlay behind useLoading for actions that block.
//
// A page that starts a named background task takes over the bar's label and
// determinate state; with no such task the bar keeps its original meaning
// (any request in flight) and its original label.
import { computed, onUnmounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { requestActivity } from "@/api/client";
import { loadingState } from "@/composables/useLoading";
import { backgroundTasks } from "@/composables/useBackgroundTask";

const { t } = useI18n();
const active = computed(() => loadingState.open.size > 0);
const busy = computed(() => requestActivity.inFlight > 0);
const shown = ref(false);
let timer: ReturnType<typeof setTimeout> | undefined;
watch(
  busy,
  (value) => {
    clearTimeout(timer);
    if (value) timer = setTimeout(() => (shown.value = true), 200);
    else shown.value = false;
  },
  { immediate: true },
);
onUnmounted(() => clearTimeout(timer));

// A named task, when one is running, is the more specific signal: the bar
// describes it rather than the generic "a request is in flight".
const named = computed(() => backgroundTasks.list[0]);
const label = computed(() => {
  const n = named.value;
  if (!n) return t("common.checkRunning");
  // A task label is a locale key when one exists, else literal text.
  const key = `backgroundTasks.${n.label}`;
  return t(key) === key ? n.label : t(key);
});
const determinate = computed(() => typeof named.value?.progress === "number");
</script>

<template>
  <v-fade-transition>
    <v-progress-linear
      v-if="shown"
      :indeterminate="!determinate"
      :model-value="determinate ? named!.progress : undefined"
      color="primary"
      height="3"
      class="loading-bar"
      :aria-label="label"
    >
      <span v-if="determinate" class="loading-bar__label">{{ label }}</span>
    </v-progress-linear>
  </v-fade-transition>
  <v-overlay
    :model-value="active"
    persistent
    class="align-center justify-center"
    scrim="surface"
  >
    <v-progress-circular indeterminate color="primary" size="48" />
  </v-overlay>
</template>

<style scoped>
.loading-bar {
  position: fixed;
  inset-block-start: 0;
  inset-inline: 0;
  z-index: 2010;
}
.loading-bar__label {
  position: absolute;
  inset-inline-end: 8px;
  inset-block-start: 4px;
  font-size: 11px;
  line-height: 1;
  opacity: 0.85;
}
</style>
