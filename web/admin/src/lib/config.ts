// Runtime config for the admin console (window.__PROXCLOUD__ from /config.js).
// Dev falls back to the local docker-compose stack (API :8080, Keycloak :8090 realm `proxcloud`).
export type ProxCloudConfig = {
  apiUrl: string
  authIssuer: string
  authClientId: string
  authScope: string
}

declare global {
  interface Window {
    __PROXCLOUD__?: Partial<ProxCloudConfig>
  }
}

const defaults: ProxCloudConfig = {
  apiUrl: "http://localhost:8080/api/v1",
  authIssuer: "http://localhost:8090/realms/proxcloud",
  authClientId: "proxcloud-admin",
  authScope: "openid profile email",
}

export const config: ProxCloudConfig = { ...defaults, ...(window.__PROXCLOUD__ ?? {}) }
