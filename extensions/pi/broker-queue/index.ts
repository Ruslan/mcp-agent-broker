import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { readFile } from "node:fs/promises";
import { homedir } from "node:os";
import { BrokerClient, extractTask } from "./broker-client";

type InstructionKind = "coder" | "reviewer";

type ActiveTask = {
  role: string;
  taskId: string;
  taskMd: string;
  instructions: InstructionKind;
};

function firstPickedTask(payload: any): { taskId: string; taskMd?: string; role?: string } | null {
  const list = payload?.tasks ?? payload?.items ?? payload?.data ?? payload;
  if (!Array.isArray(list) || list.length === 0) return null;
  const t = list[0];
  const taskId = t?.task_id ?? t?.id;
  if (!taskId) return null;
  return {
    taskId,
    taskMd: t?.task_md ?? t?.description,
    role: t?.role,
  };
}

function taskMdFromDetails(payload: any): string {
  return payload?.task_md ?? payload?.task?.task_md ?? payload?.description ?? payload?.task?.description ?? "";
}

function isAbortError(e: any): boolean {
  return e?.name === "AbortError";
}

function abortError(): Error {
  const e = new Error("Aborted");
  e.name = "AbortError";
  return e;
}

function abortableSleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(abortError());
      return;
    }

    const onAbort = () => {
      clearTimeout(timer);
      reject(abortError());
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

export default function (pi: ExtensionAPI) {
  let activeTask: ActiveTask | null = null;
  let waitLoopRunning = false;
  let stopRequested = false;
  let waitAbort: AbortController | null = null;
  let autoContinue = false;
  let loopRole: string | null = null;
  let loopInstructions: InstructionKind | null = null;
  let loopCommandName: string | null = null;

  const brokerReady = BrokerClient.fromDefaultConfig();
  const home = homedir();
  const coderInstructionsPath = `${home}/.pi/agent/extensions/broker-queue/instructions-coder.md`;
  const reviewerInstructionsPath = `${home}/.pi/agent/extensions/broker-queue/instructions-reviewer.md`;

  async function loadGeneralInstructions(kind: InstructionKind): Promise<string> {
    const p = kind === "reviewer" ? reviewerInstructionsPath : coderInstructionsPath;
    try {
      return (await readFile(p, "utf-8")).trim();
    } catch {
      return kind === "reviewer"
        ? "Reviewer: find real issues, explain impact, cite concrete locations, keep report concise."
        : "Coder: implement requested scope, verify when possible, and report concise factual results.";
    }
  }

  function extractSolutionBlock(content: string): { taskId: string; resultMd: string } | null {
    const re = /<solution\s+task_id="([^"\n]+)">\s*([\s\S]*?)\s*<\/solution>/i;
    const m = content.match(re);
    if (!m) return null;
    return { taskId: m[1], resultMd: m[2] };
  }

  async function publishTask(task: ActiveTask) {
    const generalInstructions = await loadGeneralInstructions(task.instructions);

    const msg = [
      `Task received from role '${task.role}'.`,
      "",
      "General instructions:",
      "```md",
      generalInstructions,
      "```",
      "",
      `Task ID: ${task.taskId}`,
      "",
      "Task text:",
      "```md",
      task.taskMd || "(empty task_md)",
      "```",
      "",
      "To solve, reply with exactly this template:",
      "```xml",
      `<solution task_id=\"${task.taskId}\">`,
      "# Result",
      "...markdown result...",
      "</solution>",
      "```",
    ].join("\n");

    pi.sendUserMessage(msg);
  }

  async function waitLoop(
    role: string,
    instructions: InstructionKind,
    notify: (m: string, l?: "info" | "warning" | "error") => void,
  ) {
    waitLoopRunning = true;
    stopRequested = false;
    notify(`Started wait loop for role: ${role} (${instructions})`, "info");

    let retryDelayMs = 1000;
    const maxRetryDelayMs = 60000;

    try {
      const broker = await brokerReady;

      while (!stopRequested && !activeTask) {
        try {
          waitAbort = new AbortController();
          await broker.ensureInitialized(waitAbort.signal);
          const decoded = await broker.listenRole(role, "wait", waitAbort.signal);
          const task = extractTask(decoded);

          if (!task) {
            notify("No task payload recognized, retrying wait", "warning");
            continue;
          }

          activeTask = { ...task, instructions };
          await publishTask(activeTask);
          notify(`Task received: ${task.taskId}`, "info");
          retryDelayMs = 1000;
          return;
        } catch (e: any) {
          if (stopRequested) return;
          if (isAbortError(e)) return;
          const delaySec = Math.ceil(retryDelayMs / 1000);
          notify(`Wait call ended (${e?.message || e}); retry in ${delaySec}s`, "warning");

          waitAbort = new AbortController();
          try {
            await abortableSleep(retryDelayMs, waitAbort.signal);
          } catch (sleepError: any) {
            if (stopRequested || isAbortError(sleepError)) return;
            throw sleepError;
          }
          retryDelayMs = Math.min(retryDelayMs * 2, maxRetryDelayMs);
        } finally {
          waitAbort = null;
        }
      }
    } finally {
      waitLoopRunning = false;
      if (stopRequested) notify("loop stopped", "info");
    }
  }

  function parseRoleArg(args: string | undefined, fallbackRole: string): string {
    const role = (args || "").trim();
    return role || fallbackRole;
  }

  async function startRoleLoop(role: string, instructions: InstructionKind, ctx: any, commandName: string) {
    if (activeTask) {
      ctx.ui.notify(`Already have active task ${activeTask.taskId}`, "warning");
      return;
    }
    if (waitLoopRunning) {
      ctx.ui.notify("Wait loop is already running", "warning");
      return;
    }

    // Reserve the loop slot before any async preflight to avoid two quick /c1 or /r1
    // invocations racing into two broker waits.
    waitLoopRunning = true;
    stopRequested = false;
    autoContinue = true;
    loopRole = role;
    loopInstructions = instructions;
    loopCommandName = commandName;

    const preflightAbort = new AbortController();
    waitAbort = preflightAbort;
    const preflightTimer = setTimeout(() => preflightAbort.abort(), 15000);

    try {
      const broker = await brokerReady;
      await broker.ensureInitialized(preflightAbort.signal);
      const pickedPayload = await broker.listPickedTasks(role, preflightAbort.signal);
      const picked = firstPickedTask(pickedPayload);
      if (picked) {
        let taskMd = picked.taskMd ?? "";
        if (!taskMd) {
          try {
            taskMd = taskMdFromDetails(await broker.getTask(picked.taskId, preflightAbort.signal));
          } catch (e: any) {
            if (stopRequested) throw e;
            ctx.ui.notify(`Picked task detail fetch failed (${e?.message || e}); using lightweight payload`, "warning");
          }
        }

        activeTask = {
          role: picked.role || role,
          taskId: picked.taskId,
          taskMd,
          instructions,
        };
        await publishTask(activeTask);
        ctx.ui.notify(
          `Found already picked task ${activeTask.taskId}. Please submit solution with <solution ...> template.`,
          "warning",
        );
        waitLoopRunning = false;
        return;
      }
    } catch (e: any) {
      if (stopRequested) {
        ctx.ui.notify("loop stopped", "info");
      } else {
        const reason = isAbortError(e) ? "preflight timed out or was aborted" : e?.message || e;
        ctx.ui.notify(`Picked-task check failed (${reason}), continuing to wait`, "warning");
      }
    } finally {
      clearTimeout(preflightTimer);
      if (waitAbort === preflightAbort) waitAbort = null;
    }

    if (stopRequested) {
      waitLoopRunning = false;
      return;
    }

    void waitLoop(role, instructions, (m, l = "info") => ctx.ui.notify(`[/${commandName}] ${m}`, l));
  }

  pi.registerCommand("c1", {
    description: "Wait for one task from role queue (default role: coder, coder instructions)",
    handler: async (args, ctx) => {
      const role = parseRoleArg(args, "coder");
      await startRoleLoop(role, "coder", ctx, "c1");
    },
  });

  pi.registerCommand("r1", {
    description: "Wait for one task from role queue (default role: reviewer, reviewer instructions)",
    handler: async (args, ctx) => {
      const role = parseRoleArg(args, "reviewer");
      await startRoleLoop(role, "reviewer", ctx, "r1");
    },
  });

  pi.registerCommand("cstop", {
    description: "Stop wait loop",
    handler: async (_args, ctx) => {
      stopRequested = true;
      autoContinue = false;
      waitAbort?.abort();
      ctx.ui.notify("Stop requested", "info");
    },
  });

  pi.registerCommand("cstatus", {
    description: "Show queue loop state",
    handler: async (_args, ctx) => {
      if (activeTask) {
        ctx.ui.notify(`Active task: ${activeTask.taskId} (${activeTask.role})`, "info");
        return;
      }
      ctx.ui.notify(waitLoopRunning ? "Waiting for next task" : "Idle", "info");
    },
  });

  pi.registerCommand("cbump", {
    description: "Resend full prompt for current active task",
    handler: async (_args, ctx) => {
      if (!activeTask) {
        ctx.ui.notify("No active task to bump", "warning");
        return;
      }
      await publishTask(activeTask);
      ctx.ui.notify(`Re-sent task ${activeTask.taskId}`, "info");
    },
  });

  pi.on("message_end", async (event, ctx) => {
    if (event.message.role !== "assistant") return;
    if (!activeTask) return;

    const text = (event.message.content || [])
      .filter((p: any) => p.type === "text")
      .map((p: any) => p.text || "")
      .join("\n");

    const solution = extractSolutionBlock(text);
    if (!solution) return;

    if (solution.taskId !== activeTask.taskId) {
      ctx.ui.notify(`Ignoring solution for task ${solution.taskId}; waiting ${activeTask.taskId}`, "warning");
      return;
    }

    try {
      const broker = await brokerReady;
      await broker.ensureInitialized();
      await broker.solveTask(solution.taskId, solution.resultMd);
      ctx.ui.notify(`Solved task ${solution.taskId}`, "info");
      activeTask = null;

      if (autoContinue && loopRole && loopInstructions && loopCommandName && !waitLoopRunning) {
        void waitLoop(
          loopRole,
          loopInstructions,
          (m, l = "info") => ctx.ui.notify(`[/${loopCommandName}] ${m}`, l),
        );
      }
    } catch (e: any) {
      ctx.ui.notify(`Failed to submit solution: ${e?.message || e}`, "error");
    }
  });
}
