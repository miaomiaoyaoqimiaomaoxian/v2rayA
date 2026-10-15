<script setup lang="ts">
// The policy dialog of one outbound group: which mode it uses, and the two
// thresholds the "any" mode acts on. Changing them takes effect on the next
// decision, so nothing has to restart.
import { reactive, watch } from "vue";
import { useI18n } from "vue-i18n";
import type { PluginGroupStatus } from "@/api";

const props = defineProps<{
  group: PluginGroupStatus;
  busy: boolean;
}>();

const emit = defineEmits<{
  (e: "close"): void;
  (
    e: "save",
    policy: {
      mode: string;
      failThreshold: number;
      maxSwitches: number;
      autoTest: boolean;
    },
  ): void;
}>();

const { t } = useI18n();

const modes = [
  { value: "any", title: t("plugins.modeAny") },
  { value: "pinned", title: t("plugins.modePinned") },
];

const form = reactive({
  mode: props.group.mode,
  failThreshold: props.group.failThreshold,
  maxSwitches: props.group.maxSwitches,
  autoTest: false,
});

// The dialog is reused across groups, so re-seed when the group changes.
watch(
  () => props.group.outbound,
  () => {
    form.mode = props.group.mode;
    form.failThreshold = props.group.failThreshold;
    form.maxSwitches = props.group.maxSwitches;
  },
);

function save() {
  emit("save", {
    mode: form.mode,
    failThreshold: Number(form.failThreshold),
    maxSwitches: Number(form.maxSwitches),
    autoTest: form.autoTest,
  });
}
</script>

<template>
  <v-dialog
    :model-value="true"
    max-width="520"
    @update:model-value="emit('close')"
  >
    <v-card rounded="xl">
      <v-card-item>
        <v-card-title>{{
          t("plugins.policyTitle", { name: group.outbound })
        }}</v-card-title>
        <v-card-subtitle>{{ t("plugins.policySubtitle") }}</v-card-subtitle>
      </v-card-item>

      <v-card-text>
        <v-select
          v-model="form.mode"
          :items="modes"
          :label="t('plugins.mode')"
          item-title="title"
          item-value="value"
          variant="outlined"
          density="compact"
          class="mb-4"
        />

        <v-text-field
          v-model.number="form.failThreshold"
          type="number"
          min="1"
          max="50"
          :label="t('plugins.failThreshold')"
          :hint="t('plugins.failThresholdHint')"
          persistent-hint
          variant="outlined"
          density="compact"
          class="mb-4"
        />

        <v-text-field
          v-model.number="form.maxSwitches"
          type="number"
          min="1"
          max="50"
          :label="t('plugins.maxSwitches')"
          :hint="t('plugins.maxSwitchesHint')"
          persistent-hint
          variant="outlined"
          density="compact"
          class="mb-4"
        />

        <v-switch
          v-model="form.autoTest"
          :label="t('plugins.autoTest')"
          :hint="t('plugins.autoTestHint')"
          persistent-hint
          color="primary"
          hide-details
        />
      </v-card-text>

      <v-card-actions>
        <v-spacer />
        <v-btn variant="text" @click="emit('close')">{{
          t("operations.cancel")
        }}</v-btn>
        <v-btn variant="flat" color="primary" :disabled="busy" @click="save">{{
          t("operations.save")
        }}</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>
