import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";
import { captureFileReference } from "../../../shared/chat-file-reference";
import { FileAnnotationContext } from "./FileAnnotationPreview";
import { ChatAnnotationSummary } from "./ChatAnnotationSummary";

const request=vi.hoisted(()=>vi.fn());
vi.mock("../../lib/host-clients",()=>({clientForSessionHost:()=>({POST:request})}));
vi.mock("../../lib/workspace-file-events",()=>({subscribeWorkspaceFileChanges:()=>()=>{}}));

const ref=captureFileReference({path:"src/a.ts",side:"file",line:1,endLine:1,lines:[{kind:"context",oldNo:1,newNo:1,text:"captured();"}]},"session");
function setup(){const client=new QueryClient({defaultOptions:{queries:{retry:false}}});render(<QueryClientProvider client={client}><FileAnnotationContext.Provider value={{sessionId:"session",hostId:"remote-one"}}><ChatAnnotationSummary annotations={[{id:ref.id,text:ref.path,fileReference:ref}]} /></FileAnnotationContext.Provider></QueryClientProvider>);return client;}
describe("captured file source state",()=>{
 it("loads only when opened, revalidates invalidation, and preserves captured code",async()=>{
  request.mockResolvedValue({data:{state:"changed"}});
  const client=setup();
  expect(request).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button",{name:"1 annotation"}));
  await waitFor(()=>expect(screen.getByRole("status")).toHaveTextContent("Source changed"));
  expect(screen.getByText("captured();")).toBeInTheDocument();
  expect(screen.queryByRole("button",{name:/src\/a.ts/})).not.toBeInTheDocument();
  request.mockResolvedValue({data:{state:"unavailable"}});
  await client.invalidateQueries({queryKey:["file-annotation-source","remote-one","session"]});
  await waitFor(()=>expect(screen.getByRole("status")).toHaveTextContent("Source unavailable"));
  expect(ref.lines[0].text).toBe("captured();");
 });
});
