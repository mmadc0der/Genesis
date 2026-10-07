import { describe, expect, it } from "vitest";
import { canBack, canForward, createViewStack, goBack, goForward, pushView } from "./view-stack";

describe("view stack", () => {
  it("starts with one frame and no history", () => {
    const stack = createViewStack({ id: "wire" });
    expect(canBack(stack)).toBe(false);
    expect(canForward(stack)).toBe(false);
    expect(goBack(stack)).toBe(stack);
    expect(goForward(stack)).toBe(stack);
  });

  it("pushes, goes back, and goes forward", () => {
    const pushed = pushView(createViewStack({ id: "wire" }), { id: "run" });
    expect(pushed.index).toBe(1);
    expect(pushed.direction).toBe(1);
    const back = goBack(pushed);
    expect(back.index).toBe(0);
    expect(back.direction).toBe(-1);
    expect(canForward(back)).toBe(true);
    expect(goForward(back).index).toBe(1);
  });

  it("drops forward frames when pushing from the past", () => {
    let stack = pushView(createViewStack({ id: "wire" }), { id: "a" });
    stack = goBack(stack);
    stack = pushView(stack, { id: "b" });
    expect(stack.frames.map((frame) => frame.id)).toEqual(["wire", "b"]);
  });
});
