export interface ViewFrame {
  id: string;
}

export interface ViewStack {
  frames: ViewFrame[];
  index: number;
  direction: -1 | 1;
}

export function createViewStack(frame: ViewFrame): ViewStack {
  return { frames: [frame], index: 0, direction: 1 };
}

export function canBack(stack: ViewStack) {
  return stack.index > 0;
}

export function canForward(stack: ViewStack) {
  return stack.index < stack.frames.length - 1;
}

export function goBack(stack: ViewStack): ViewStack {
  if (!canBack(stack)) return stack;
  return { ...stack, index: stack.index - 1, direction: -1 };
}

export function goForward(stack: ViewStack): ViewStack {
  if (!canForward(stack)) return stack;
  return { ...stack, index: stack.index + 1, direction: 1 };
}

export function pushView(stack: ViewStack, frame: ViewFrame): ViewStack {
  const frames = stack.frames.slice(0, stack.index + 1);
  frames.push(frame);
  return { frames, index: frames.length - 1, direction: 1 };
}

export function navigateToFrame(stack: ViewStack, frameId: string): ViewStack {
  if (stack.frames[stack.index]?.id === frameId) return stack;
  const targetIndex = stack.frames.findIndex((f) => f.id === frameId);
  if (targetIndex !== -1) {
    return {
      ...stack,
      index: targetIndex,
      direction: targetIndex < stack.index ? -1 : 1,
    };
  }
  return pushView(stack, { id: frameId });
}

