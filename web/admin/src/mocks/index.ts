// Entry point: registers all handlers and installs the mock dispatcher into
// the API client. Loaded dynamically from main.tsx when VITE_MOCK=1.
import { setMockHandler } from "@/lib/api"
import { dispatch } from "./router"

import "./handlers/dashboard"
import "./handlers/clients"
import "./handlers/system"
import "./handlers/audit"

export function enableMocks() {
  setMockHandler(dispatch)
  console.info("%c[mock] ProxCloud admin mock mode — all API calls served from src/mocks fixtures", "color:#FF5C00;font-weight:bold")
}
