<script setup lang="ts">
// The plugins page's management dialog: adding, removing, importing and
// exporting plugins.
//
// 内置插件编译在 v2rayA 里，只能停用不能卸载；能装卸的是将来接入的外部插件。
// 这里把两条路径分开呈现，避免用户对一个内置插件点"卸载"却只得到一句拒绝。
import { ref } from "vue";
import { useI18n } from "vue-i18n";
import {
  mdiDelete,
  mdiFileDownloadOutline,
  mdiFileUploadOutline,
  mdiPlus,
} from "@mdi/js";
import type { PluginRow } from "./model";

defineOptions({ name: "PluginManageDialog" });
const props = defineProps<{
  plugins: PluginRow[];
  busy: boolean;
}>();
const emit = defineEmits<{
  (e: "close"): void;
  (e: "add", name: string): void;
  (e: "uninstall", name: string): void;
  (e: "import"): void;
  (e: "export"): void;
}>();

const { t } = useI18n();
const tab = ref<"builtin" | "external">("builtin");
const newName = ref("");

const builtin = () => props.plugins.filter((p) => p.builtin);
const external = () => props.plugins.filter((p) => !p.builtin);

function submitAdd() {
  const name = newName.value.trim();
  if (!name) return;
  emit("add", name);
  newName.value = "";
}
</script>

<template>
  <v-dialog
    :model-value="true"
    max-width="640"
    @update:model-value="emit('close')"
  >
    <v-card rounded="xl">
      <v-card-item>
        <v-card-title>{{ t("plugins.manage") }}</v-card-title>
        <v-card-subtitle>{{ t("plugins.manageSubtitle") }}</v-card-subtitle>
      </v-card-item>

      <v-card-text>
        <v-tabs v-model="tab" density="compact" class="mb-4">
          <v-tab value="builtin">{{ t("plugins.builtin") }}</v-tab>
          <v-tab value="external">{{ t("plugins.external") }}</v-tab>
        </v-tabs>

        <v-window v-model="tab">
          <v-window-item value="builtin">
            <v-alert type="info" variant="tonal" density="compact" class="mb-3">
              {{ t("plugins.builtinHint") }}
            </v-alert>
            <v-list rounded="lg" bg-color="surface-container-low">
              <v-list-item
                v-for="p in builtin()"
                :key="p.name"
                :title="p.name"
                :subtitle="p.description"
              >
                <template #append>
                  <v-chip
                    size="small"
                    variant="tonal"
                    :color="p.running ? 'success' : undefined"
                  >
                    {{
                      p.running
                        ? t("plugins.stateRunning")
                        : t("plugins.stateStopped")
                    }}
                  </v-chip>
                </template>
              </v-list-item>
              <v-list-item v-if="!builtin().length">
                <span class="text-on-surface-variant">{{
                  t("plugins.none")
                }}</span>
              </v-list-item>
            </v-list>
          </v-window-item>

          <v-window-item value="external">
            <div class="d-flex ga-2 mb-3">
              <v-text-field
                v-model="newName"
                :label="t('plugins.addName')"
                :placeholder="t('plugins.addNamePlaceholder')"
                density="compact"
                variant="outlined"
                hide-details
                class="flex-grow-1"
                @keyup.enter="submitAdd"
              />
              <v-btn
                variant="tonal"
                color="primary"
                :prepend-icon="mdiPlus"
                :disabled="busy || !newName.trim()"
                @click="submitAdd"
                >{{ t("plugins.add") }}</v-btn
              >
            </div>

            <v-list rounded="lg" bg-color="surface-container-low">
              <v-list-item
                v-for="p in external()"
                :key="p.name"
                :title="p.name"
                :subtitle="p.description"
              >
                <template #append>
                  <v-btn
                    :icon="mdiDelete"
                    variant="text"
                    density="compact"
                    color="error"
                    :disabled="busy"
                    @click="emit('uninstall', p.name)"
                  />
                </template>
              </v-list-item>
              <v-list-item v-if="!external().length">
                <span class="text-on-surface-variant">{{
                  t("plugins.externalNone")
                }}</span>
              </v-list-item>
            </v-list>
          </v-window-item>
        </v-window>

        <v-divider class="my-4" />

        <div class="d-flex flex-wrap ga-2">
          <v-btn
            variant="tonal"
            :prepend-icon="mdiFileUploadOutline"
            :disabled="busy"
            @click="emit('import')"
            >{{ t("plugins.import") }}</v-btn
          >
          <v-btn
            variant="tonal"
            :prepend-icon="mdiFileDownloadOutline"
            :disabled="busy || !plugins.length"
            @click="emit('export')"
            >{{ t("plugins.export") }}</v-btn
          >
        </div>
        <p class="md3-body-small text-on-surface-variant mt-2">
          {{ t("plugins.transferHint") }}
        </p>
      </v-card-text>

      <v-card-actions>
        <v-spacer />
        <v-btn variant="text" @click="emit('close')">{{
          t("operations.cancel")
        }}</v-btn>
      </v-card-actions>
    </v-card>
  </v-dialog>
</template>
