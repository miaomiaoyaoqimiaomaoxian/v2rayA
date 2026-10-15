// A registry of named background tasks, so the progress bar can say what is
// running instead of merely "a request is in flight".
//
// The vanilla bar shows for any request and labels it with the running check.
// That is wrong for two cases the subscription page hits: a subscription set
// to update manually should show that specific work, and a bulk update should
// say how far along it is rather than flickering per request.
//
// This is a plain module rather than a plugin: it is inert until something
// registers a task, and AppShell falls back to the vanilla behaviour when
// nothing is registered.
import { computed, reactive, readonly } from "vue";

export interface BackgroundTask {
  /** stable id, used to end the task */
  id: number;
  /** locale key or literal text shown next to the bar */
  label: string;
  /** 0..1, or undefined while indeterminate */
  progress?: number;
}

const state = reactive({
  /** key -> task; a key replaces its task so a repeat click does not stack */
  tasks: new Map<string, BackgroundTask>(),
});

let nextId = 1;

/**
 * begin marks a task as running under key and returns a handle whose end()
 * clears it. Tasks are keyed rather than counted so a second click on the
 * same button updates the existing bar instead of adding another.
 */
export function beginTask(key: string, label: string): { end(): void } {
  const id = nextId++;
  state.tasks.set(key, { id, label });
  return {
    end() {
      const cur = state.tasks.get(key);
      if (cur && cur.id === id) state.tasks.delete(key);
    },
  };
}

/** setProgress updates a running task; ignored once it has ended. */
export function setTaskProgress(
  key: string,
  progress: number | undefined,
  label?: string,
): void {
  const cur = state.tasks.get(key);
  if (!cur) return;
  cur.progress = progress;
  if (label !== undefined) cur.label = label;
}

/** endTask clears a task regardless of who started it. */
export function endTask(key: string): void {
  state.tasks.delete(key);
}

/** closeAllTasks drops every task; the session reset calls it. */
export function closeAllTasks(): void {
  state.tasks.clear();
}

const list = computed(() => Array.from(state.tasks.values()));

/** hasTask is true while any named task is running. */
const hasTask = computed(() => state.tasks.size > 0);

export const backgroundTasks = readonly({ list, hasTask });
