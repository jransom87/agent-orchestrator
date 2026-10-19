import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { captureFileReference } from "../../../shared/chat-file-reference";
import { FileAnnotationContext } from "./FileAnnotationPreview";
import { ChatAnnotationSummary } from "./ChatAnnotationSummary";

const request = vi.hoisted(() => vi.fn());
vi.mock("../../lib/host-clients", () => ({ clientForSessionHost: () => ({ POST: request }) }));
vi.mock("../../lib/workspace-file-events", () => ({ subscribeWorkspaceFileChanges: () => () => {} }));

const ref = captureFileReference(
	{ path: "src/a.ts", side: "file", line: 1, endLine: 1, lines: [{ kind: "context", oldNo: 1, newNo: 1, text: "captured();" }] },
	"session",
);
beforeEach(() => request.mockReset());
function setup(onOpen?: () => void, generation = { current: 0 }, reference = ref) {
	const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
	render(
		<QueryClientProvider client={client}>
			<FileAnnotationContext.Provider value={{ sessionId: "session", hostId: "remote-one", onOpen, generation }}>
				<ChatAnnotationSummary annotations={[{ id: reference.id, text: reference.path, fileReference: reference }]} />
			</FileAnnotationContext.Provider>
		</QueryClientProvider>,
	);
	return client;
}
describe("captured file source state", () => {
	it("loads only when opened, revalidates invalidation, and preserves captured code", async () => {
		request.mockResolvedValue({ data: { state: "changed" } });
		const client = setup();
		expect(request).not.toHaveBeenCalled();
		fireEvent.click(screen.getByRole("button", { name: "1 annotation" }));
		await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Source changed"));
		expect(screen.getByText("captured();")).toBeInTheDocument();
		expect(screen.queryByRole("button", { name: /src\/a.ts/ })).not.toBeInTheDocument();
		request.mockResolvedValue({ data: { state: "unavailable" } });
		await client.invalidateQueries({ queryKey: ["file-annotation-source", "remote-one", "session"] });
		await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Source unavailable"));
		expect(ref.lines[0].text).toBe("captured();");
	});
});

it("rechecks before opening and cancels an older click", async () => {
	request.mockResolvedValue({ data: { state: "unchanged", path: ref.path, line: 1, endLine: 1 } });
	const open = vi.fn(),
		generation = { current: 0 };
	setup(open, generation);
	fireEvent.click(screen.getByRole("button", { name: "1 annotation" }));
	const button = await screen.findByRole("button", { name: /^Open selected lines:/ });
	let finish!: (value: unknown) => void;
	request.mockImplementationOnce(
		() =>
			new Promise((resolve) => {
				finish = resolve;
			}),
	);
	fireEvent.click(button);
	generation.current++;
	finish({ data: { state: "unchanged", path: ref.path, line: 1, endLine: 1 } });
	await waitFor(() => expect(button).not.toHaveAttribute("aria-disabled", "true"));
	expect(open).not.toHaveBeenCalled();
	request.mockResolvedValueOnce({ data: { state: "changed" } });
	fireEvent.click(button);
	await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Source changed"));
	expect(open).not.toHaveBeenCalled();
});

it("opens from the whole annotation and supports keyboard activation", async () => {
	const user = userEvent.setup();
	request.mockResolvedValue({ data: { state: "unchanged", path: ref.path, line: 1, endLine: 1 } });
	const open = vi.fn();
	setup(open);
	await user.click(screen.getByRole("button", { name: "1 annotation" }));
	await screen.findByRole("button", { name: /^Open selected lines: src\/a.ts/ });
	expect(screen.queryByText("Open selected lines")).not.toBeInTheDocument();
	await user.click(screen.getByText("captured();"));
	await waitFor(() => expect(open).toHaveBeenCalledOnce());
	await user.click(screen.getByRole("button", { name: "1 annotation" }));
	(await screen.findByRole("button", { name: /^Open selected lines: src\/a.ts/ })).focus();
	await user.keyboard("{Enter}");
	await waitFor(() => expect(open).toHaveBeenCalledTimes(2));
});

it("keeps capture-truncated references static even if the resolver reports unchanged", async () => {
	request.mockResolvedValue({ data: { state: "unchanged" } });
	setup(vi.fn(), { current: 0 }, { ...ref, truncated: true });
	fireEvent.click(screen.getByRole("button", { name: "1 annotation" }));
	await waitFor(() => expect(request).toHaveBeenCalledOnce());
	expect(screen.queryByRole("button", { name: /^Open selected lines:/ })).not.toBeInTheDocument();
	expect(screen.getByText("Captured selection truncated")).toBeInTheDocument();
});
