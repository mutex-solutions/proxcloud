// Route module for /p/:pid/databases/:resourceId — the page itself lives next to the list page
// so the two share the database data layer, dialogs and converters without an import cycle.
export { DatabaseClusterDetailPage as default } from "./DatabasesPage"
