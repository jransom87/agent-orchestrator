import { act, renderHook } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { FileAnnotationRange } from "../../../shared/chat-file-reference";
import { useFileAnnotationReveal } from "./useFileAnnotationReveal";

function setup(markup: string, range: FileAnnotationRange, offscreen = false) {
	const container = document.createElement("div");
	document.body.append(container);
	const host = document.createElement("diffs-container");
	container.append(host);
	const root = host.attachShadow({ mode: "open" });
	root.innerHTML = markup;
	const scroll = vi.fn();
	for (const el of root.querySelectorAll<HTMLElement>("[data-line]")) {
		el.scrollIntoView = scroll;
		el.getBoundingClientRect = () => ({
			top: offscreen ? 5000 : 10,
			bottom: offscreen ? 5020 : 30,
			left: 0,
			right: 100,
			width: 100,
			height: 20,
			x: 0,
			y: 0,
			toJSON: () => ({}),
		});
	}
	const containerRef = { current: container };
	const hook = renderHook(({ selection }) => useFileAnnotationReveal(containerRef, selection), { initialProps: { selection: range } });
	act(() => hook.result.current.reveal());
	return {
		...hook,
		scroll,
		cleanup: () => {
			hook.unmount();
			container.remove();
		},
	};
}
const range: FileAnnotationRange = { requestKey: 1, line: 2, endLine: 2, side: "file", lines: [{ kind: "context", oldNo: 2, newNo: 2, text: "captured();" }] };
describe("file annotation reveal", () => {
	it("highlights a visible verified range without scrolling", () => {
		const view = setup('<div data-line="2" data-line-type="context">captured();</div>', range);
		expect(view.result.current.selectedLines).toMatchObject({ start: 2, end: 2 });
		expect(view.scroll).not.toHaveBeenCalled();
		view.cleanup();
	});
	it("scrolls offscreen verified rows and processes a repeated request", () => {
		const view = setup('<div data-line="2" data-line-type="context">captured();</div>', range, true);
		expect(view.scroll).toHaveBeenCalledOnce();
		view.rerender({ selection: { ...range, requestKey: 2 } });
		act(() => view.result.current.reveal());
		expect(view.scroll).toHaveBeenCalledTimes(2);
		view.cleanup();
	});
	it("refuses to highlight code changed between verification and rendering", () => {
		const view = setup('<div data-line="2" data-line-type="context">different();</div>', range, true);
		expect(view.result.current.changed).toBe(true);
		expect(view.result.current.selectedLines).toBeNull();
		expect(view.scroll).not.toHaveBeenCalled();
		view.cleanup();
	});
	it("keeps old-side line numbers in a unified diff and selects across both sides", () => {
		const view = setup(
			'<div data-line="2" data-line-type="change-deletion">removed();</div><div data-line="3" data-line-type="change-addition">added();</div>',
			{
				...range,
				side: "old",
				endLine: 3,
				lines: [
					{ kind: "del", oldNo: 2, newNo: null, text: "removed();" },
					{ kind: "add", oldNo: null, newNo: 3, text: "added();" },
				],
			},
		);
		expect(view.result.current.selectedLines).toEqual({ start: 2, end: 3, side: "deletions", endSide: "additions" });
		view.cleanup();
	});
});

it("clears an existing highlight when a live redraw replaces its code", () => {
	const view = setup('<div data-line="2" data-line-type="context">captured();</div>', range);
	expect(view.result.current.selectedLines).not.toBeNull();
	const host = document.querySelector("diffs-container")!;
	host.shadowRoot!.querySelector("[data-line]")!.textContent = "different();";
	act(() => view.result.current.reveal());
	expect(view.result.current.selectedLines).toBeNull();
	expect(view.result.current.changed).toBe(true);
	expect(view.scroll).not.toHaveBeenCalled();
	view.cleanup();
});
