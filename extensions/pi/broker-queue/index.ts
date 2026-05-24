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

    try {
      const broker = await brokerReady;
      await broker.ensureInitialized();

      while (!stopRequested && !activeTask) {
        try {
          waitAbort = new AbortController();
          const decoded = await broker.listenRole(role, "wait", waitAbort.signal);
          const task = extractTask(decoded);

          if (!task) {
            notify("No task payload recognized, retrying wait", "warning");
            continue;
          }

          activeTask = { ...task, instructions };
          await publishTask(activeTask);
          notify(`Task received: ${task.taskId}`, "info");
          return;
        } catch (e: any) {
          if (stopRequested) return;
          if (e?.name === "AbortError") return;
          notify(`Wait call ended (${e?.message || e}); retrying`, "warning");
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

  function startRoleLoop(role: string, instructions: InstructionKind, ctx: any, commandName: string) {
    if (activeTask) {
      ctx.ui.notify(`Already have active task ${activeTask.taskId}`, "warning");
      return;
    }
    if (waitLoopRunning) {
      ctx.ui.notify("Wait loop is already running", "warning");
      return;
    }
    autoContinue = true;
    loopRole = role;
    loopInstructions = instructions;
    loopCommandName = commandName;
    void waitLoop(role, instructions, (m, l = "info") => ctx.ui.notify(`[/${commandName}] ${m}`, l));
  }

  pi.registerCommand("c1", {
    description: "Wait for one task from role queue (default role: coder, coder instructions)",
    handler: async (args, ctx) => {
      const role = parseRoleArg(args, "coder");
      startRoleLoop(role, "coder", ctx, "c1");
    },
  });

  pi.registerCommand("r1", {
    description: "Wait for one task from role queue (default role: reviewer, reviewer instructions)",
    handler: async (args, ctx) => {
      const role = parseRoleArg(args, "reviewer");
      startRoleLoop(role, "reviewer", ctx, "r1");
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
