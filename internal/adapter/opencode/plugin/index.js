// otag plugin for OpenCode v2 (Plugin.define shape, no imports from @opencode/plugin).
// Loaded per launch through OPENCODE_CONFIG_CONTENT; options come from otag:
//   { otag: "/abs/otag", run: "<run id>", instructions: "/abs/<run dir>/opencode/instructions.md" }
// - session "context": appends otag's system note to every model call (the config
//   `instructions` key is a no-op in v2).
// - permission "evaluate": asks otag's guard (`otag event opencode permission.evaluate`)
//   about shell commands, edits, reads and external directories; otag answers
//   {"effect":"deny","message":...} or nothing. Any failure (otag missing, a timeout,
//   unreadable output) denies: the guard fails closed, like the bridge itself.
// - tool "execute.before": remembers which tool a call id belongs to and the shell
//   tool's workdir, so the guard judges commands from the directory they run in and
//   knows whether an external_directory request comes from an edit tool.
import { spawn } from "node:child_process"
import { readFileSync } from "node:fs"

const GUARDED = new Set(["shell", "edit", "external_directory", "read"])
const UNAVAILABLE = { effect: "deny", message: "otag guard: otag could not be asked; refusing (fail closed)" }
const TIMEOUT_MS = 6000

export default {
  id: "otag",
  async setup(ctx) {
    const opts = ctx.options || {}
    const otag = opts.otag || process.env.OTAG_BIN || "otag"
    const env = { ...process.env }
    if (opts.run) env.OTAG_RUN_ID = opts.run
    let note = ""
    try {
      note = readFileSync(opts.instructions, "utf8").trim()
    } catch {}

    const tools = new Map() // tool call id -> { tool, workdir }
    const ask = (kind, payload) =>
      new Promise((resolve) => {
        let out = ""
        let settled = false
        let timer
        const finish = (v) => {
          if (settled) return
          settled = true
          clearTimeout(timer)
          resolve(v)
        }
        let p
        try {
          p = spawn(otag, ["event", "opencode", kind], { env, stdio: ["pipe", "pipe", "ignore"] })
        } catch {
          return finish(UNAVAILABLE)
        }
        timer = setTimeout(() => {
          try {
            p.kill()
          } catch {}
          finish(UNAVAILABLE)
        }, TIMEOUT_MS)
        p.on("error", () => finish(UNAVAILABLE))
        p.stdout.on("data", (d) => (out += d))
        // otag answers every evaluate with a JSON object ({} = no opinion): a nonzero
        // exit, a signal or no answer at all means otag could not decide, and denies.
        p.on("close", (code, signal) => {
          if (code !== 0 || signal || !out.trim()) return finish(UNAVAILABLE)
          try {
            finish(JSON.parse(out))
          } catch {
            finish(UNAVAILABLE)
          }
        })
        p.stdin.on("error", () => {})
        p.stdin.end(JSON.stringify(payload))
      })

    await ctx.tool.hook("execute.before", (e) => {
      if (!e || !e.id) return
      const workdir = e.input && typeof e.input.workdir === "string" ? e.input.workdir : undefined
      tools.set(e.id, { tool: e.tool, workdir })
      if (tools.size > 1000) tools.delete(tools.keys().next().value)
    })

    await ctx.permission.hook("evaluate", async (e) => {
      if (!e || e.effect === "deny" || !GUARDED.has(e.action)) return
      const callID = e.source && e.source.id
      const call = (callID && tools.get(callID)) || {}
      const d = await ask("permission.evaluate", {
        sessionID: e.sessionID,
        action: e.action,
        resources: e.resources || [],
        tool: call.tool,
        workdir: call.workdir,
        callID,
        effect: e.effect,
      })
      if (d && d.effect === "deny") {
        e.effect = "deny"
        if (d.message) e.message = d.message
      }
    })

    if (note) {
      await ctx.session.hook("context", (e) => {
        if (e && Array.isArray(e.system)) e.system.push({ type: "text", text: note })
      })
    }
  },
}
