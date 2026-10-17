import { useCallback, useEffect, useRef, useState, type RefObject } from "react";
import type { SelectedLineRange } from "@pierre/diffs";
import type { FileAnnotationRange } from "../../../shared/chat-file-reference";

export function useFileAnnotationReveal(container: RefObject<HTMLElement | null>, range?: FileAnnotationRange) {
	const [selectedLines, setSelectedLines] = useState<SelectedLineRange | null>(null);
	const [changed, setChanged] = useState(false);
	const consumed = useRef<number | undefined>(undefined);
	const reveal = useCallback(() => {
		if (!range) return;
		const alreadyRevealed = consumed.current === range.requestKey;
		const roots = [...(container.current?.querySelectorAll("diffs-container") ?? [])].flatMap((host) => (host.shadowRoot ? [host.shadowRoot] : []));
		const expected = range.lines;
		const oldSide = (row: (typeof expected)[number]) =>
			row.side === "old" || (row.side !== "new" && (row.kind === "del" || (range.side === "old" && row.kind !== "add")));
		const oldRows = new Map<number, HTMLElement>();
		const newRows = new Map<number, HTMLElement>();
		for (const element of roots.flatMap((root) => [...root.querySelectorAll<HTMLElement>("[data-line][data-line-type]")])) {
			const deletion = element.dataset.lineType === "change-deletion" || Boolean(element.closest("[data-deletions]"));
			const number = Number(element.dataset.line);
			if (deletion) oldRows.set(number, element);
			else newRows.set(number, element);
			if (!deletion && element.dataset.lineType !== "change-addition") {
				const oldNumber = Number(element.dataset.altLine ?? element.dataset.line);
				if (!oldRows.has(oldNumber)) oldRows.set(oldNumber, element);
			}
		}
		const elements: HTMLElement[] = [];
		for (const row of expected) {
			const old = oldSide(row);
			const number = old ? row.oldNo : row.newNo;
			if (number == null) continue;
			const element = (old ? oldRows : newRows).get(number);
			if (!element || element.textContent !== row.text) {
				if (element || alreadyRevealed) {
					setChanged(true);
					setSelectedLines(null);
					consumed.current = range.requestKey;
				}
				return;
			}
			elements.push(element);
		}
		if (!elements.length || alreadyRevealed) return;
		const side = oldSide(expected[0]) ? "deletions" : "additions";
		const endSide = oldSide(expected[expected.length - 1]) ? "deletions" : "additions";
		const first = elements[0],
			last = elements[elements.length - 1];
		consumed.current = range.requestKey;
		setSelectedLines({
			start: oldSide(expected[0]) ? expected[0].oldNo! : expected[0].newNo!,
			end: oldSide(expected[expected.length - 1]) ? expected[expected.length - 1].oldNo! : expected[expected.length - 1].newNo!,
			side,
			endSide,
		});
		let parent = container.current?.parentElement;
		while (parent && !/(auto|scroll)/.test(getComputedStyle(parent).overflowY)) parent = parent.parentElement;
		const view = parent?.getBoundingClientRect();
		const top = Math.max(0, view?.top ?? 0),
			bottom = Math.min(window.innerHeight, view?.bottom ?? window.innerHeight);
		if (first.getBoundingClientRect().top < top || last.getBoundingClientRect().bottom > bottom) first.scrollIntoView({ block: "center" });
	}, [container, range]);
	useEffect(() => {
		consumed.current = undefined;
		setChanged(false);
		setSelectedLines(null);
		const frame = requestAnimationFrame(reveal);
		const timeout = setTimeout(() => {
			if (range && consumed.current !== range.requestKey) setChanged(true);
		}, 5000);
		return () => {
			cancelAnimationFrame(frame);
			clearTimeout(timeout);
		};
	}, [range?.requestKey, reveal]);
	return { selectedLines: range && consumed.current === range.requestKey ? selectedLines : null, changed, reveal };
}
