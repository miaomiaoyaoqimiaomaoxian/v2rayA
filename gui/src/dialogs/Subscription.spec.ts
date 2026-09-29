// @vitest-environment happy-dom
import { afterEach, beforeEach, expect, test, vi } from "vitest";
import { flushPromises, type VueWrapper } from "@vue/test-utils";
import { VSelect } from "vuetify/components";
import type * as Api from "@/api";
import { patchSubscription } from "@/api";
import type { TouchResponse, TouchSubscription } from "@/api/types";
import { mountWithApp } from "@/test/mount";
import Subscription from "./Subscription.vue";

vi.mock("@/api", async (original) => ({
  ...(await original<typeof Api>()),
  patchSubscription: vi.fn(),
}));

const subscription: TouchSubscription = {
  id: 3,
  _type: "subscription",
  remarks: "Travel",
  host: "example.test",
  address: "https://example.test/subscription",
  status: "",
  info: "",
  servers: [],
  autoSelect: false,
  autoUpdateMode: "none",
  autoUpdateIntervalHour: 0,
};
let wrapper: VueWrapper;
beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(patchSubscription).mockResolvedValue({} as TouchResponse);
  wrapper = mountWithApp(Subscription, { props: { subscription } });
});
afterEach(() => wrapper.unmount());

test("requires a positive interval for this subscription only", async () => {
  wrapper
    .findComponent(VSelect)
    .vm.$emit("update:modelValue", "auto_update_at_intervals");
  await flushPromises();
  await wrapper.get('input[type="number"]').setValue("0");
  const save = wrapper
    .findAll("button")
    .find((b) => b.text() === "Save and Apply")!;
  await save.trigger("click");
  await flushPromises();
  expect(patchSubscription).not.toHaveBeenCalled();
  expect(wrapper.emitted("close")).toBeUndefined();

  await wrapper.get('input[type="number"]').setValue("24");
  await save.trigger("click");
  await flushPromises();
  expect(patchSubscription).toHaveBeenCalledWith({
    subscription: expect.objectContaining({
      id: 3,
      autoUpdateMode: "auto_update_at_intervals",
      autoUpdateIntervalHour: 24,
    }),
  });
  expect(wrapper.emitted("close")).toEqual([[true]]);
});
