// Identity + dashboard aggregates.
import { on } from "../router"
import { db } from "../db"

on("GET /admin/me", () => ({ data: db.adminMe }))
on("GET /admin/stats", () => ({ data: db.adminStats }))
