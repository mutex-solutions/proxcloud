// Route module for /p/:pid/kubernetes/:resourceId — the page itself lives next to the list page
// so the two share the cluster data layer, dialogs and converters without an import cycle.
export { KubernetesClusterDetailPage as default } from "./KubernetesPage"
