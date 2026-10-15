<script setup lang="ts">
// The detail view of one plugin: its state, its outbound groups with their
// automation policy, and — for autogroup — where its decision log lives and
// how loud that log is.
import { computed, ref, watch } from "vue";
import { useI18n } from "vue-i18n";
import { mdiPuzzle, mdiReload, mdiTune } from "@mdi/js";
import type { PluginGroupStatus, PluginRow } from "./model";
import PluginPolicyDialog from "./PluginPolicyDialog.vue";

/** The policy payload PutPluginPolicy expects. */
interface PolicyPayload {
  mode: string;
  failThreshold: number;
  maxSwitches: number;
  autoTest: boolean;
}

defineOptions({ name: "PluginDetailDialog" });
const props = defineProps<{
  plugin: PluginRow;
  busy: boolean;
  testing: string;
  logLevel: string;
  logPath: string;
  logLevels: string[];
}>();
const emit = defineEmits<{
  (e: "close"): void;
  (e: "toggle", name: string, enabled: boolean): void;
  (e: "uninstall", name: string): void;
  (e: "test", outbound: string): void;
  (e: "setLevel", level: string): void;
  (e: "savePolicy", outbound: string, policy: PolicyPayload): void;
}>();

const { t } = useI18n();
const editing = ref<PluginGroupStatus | null>(null);
const level = ref(props.logLevel);

watch(
  () => props.logLevel,
  (v) => (level.value = v),
);

const levelItems = computed(() =>
  (props.logLevels.length
    ? props.logLevels
    : ["debug", "info", "warn", "error"]
  ).map((l) => ({
    value: l,
    title: t(`plugins.logLevels.${l}`),
  })),
);

function saveLevel() {
  emit("setLevel", level.value);
}
</script>

<template>
  <v-dialog
    :model-value="true"
    max-width="720"
    scrollable
    @update:model-value="emit('close')"
  >
    <v-card rounded="xl">
      <v-card-item>
        <template #prepend>
          <v-avatar color="primary" variant="tonal" rounded="lg" size="44">
            <v-icon :icon="mdiPuzzle" size="22" />
          </v-avatar>
        </template>
        <v-card-title dir="auto">{{ plugin.name }}</v-card-title>
        <v-card-subtitle dir="ltr">{{ plugin.version }}</v-card-subtitle>
        <template #append>
          <v-switch
            :model-value="plugin.state === 'enabled'"
            color="primary"
            density="compact"
            hide-details
            :disabled="busy"
            @update:model-value="emit('toggle', plugin.name, $event === true)"
          />
        </template>
      </v-card-item>

      <v-card-text>
        <p class="md3-body-medium mb-4 text-wrap" dir="auto">
          {{ plugin.description }}
        </p>

        <v-list
          v-if="plugin.groups.length"
          class="mb-4"
          rounded="lg"
          bg-color="surface-container-low"
        >
          <v-list-subheader>{{ t("plugins.groupsTitle") }}</v-list-subheader>
          <v-list-item
            v-for="g in plugin.groups"
            :key="g.outbound"
            :title="g.outbound"
            :subtitle="
              g.mode === 'pinned'
                ? t('plugins.modePinned')
                : t('plugins.modeAny')
            "
          >
            <template #append>
              <v-chip size="small" variant="tonal" class="mr-2">
                {{ t("plugins.nodeCount", g.nodes.length) }}
              </v-chip>
              <v-btn
                variant="text"
                density="compact"
                :icon="mdiTune"
                :disabled="busy"
                @click="editing = g"
              />
              <v-btn
                variant="text"
                density="compact"
                :icon="mdiReload"
                :loading="testing === g.outbound"
                :disabled="busy"
                @click="emit('test', g.outbound)"
              />
            </template>
          </v-list-item>
        </v-list>

        <v-list rounded="lg" bg-color="surface-container-low">
          <v-list-subheader>{{ t("plugins.logTitle") }}</v-list-subheader>
          <v-list-item :title="t('plugins.logPath')">
            <template #subtitle>
              <span dir="ltr">{{
                logPath || t("plugins.logUnavailable")
              }}</span>
            </template>
          </v-list-item>
          <v-list-item>
            <v-select
              v-model="level"
              :items="levelItems"
              :label="t('plugins.logLevel')"
              :hint="t('plugins.logLevelHint')"
              persistent-hint
              density="compact"
              variant="outlined"
              @update:model-value="saveLevel"
            />
          </v-list-item>
        </v-list>
      </v-card-text>

      <v-card-actions>
        <v-btn
          variant="text"
          color="error"
          :disabled="busy || plugin.builtin"
          @click="emit('uninstall', plugin.name)"
        >
          {{ plugin.builtin ? t("plugins.builtin") : t("plugins.uninstall") }}
        </v-btn>
        <v-spacer />
        <v-btn variant="text" @click="emit('close')">{{
          t("operations.cancel")
        }}</v-btn>
      </v-card-actions>

      <PluginPolicyDialog
        v-if="editing"
        :group="editing"
        :busy="busy"
        @close="editing = null"
        @save="emit('savePolicy', editing.outbound, $event)"
      />
    </v-card>
  </v-dialog>
</template>
