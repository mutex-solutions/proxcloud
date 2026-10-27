import { StrictMode } from "react"
import { createRoot } from "react-dom/client"
import { QueryClient, QueryClientProvider } from "@tanstack/react-query"
import { Toaster } from "sonner"
import "./index.css"
import App from "./App"
import { AuthBridge, ProxCloudAuthProvider } from "./lib/auth"

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: 1, refetchOnWindowFocus: false, staleTime: 15_000 } },
})

// VITE_MOCK=1 (npm run dev:mock) boots the console against in-memory fixtures —
// no backend, no OIDC. The mock module is loaded lazily so production builds
// never include it.
const prepare =
  import.meta.env.VITE_MOCK === "1"
    ? import("./mocks").then((m) => m.enableMocks())
    : Promise.resolve()

void prepare.then(() =>
  createRoot(document.getElementById("root")!).render(
    <StrictMode>
      <ProxCloudAuthProvider>
        <AuthBridge>
          <QueryClientProvider client={queryClient}>
            <App />
            <Toaster richColors position="top-right" />
          </QueryClientProvider>
        </AuthBridge>
      </ProxCloudAuthProvider>
    </StrictMode>,
  ),
)
