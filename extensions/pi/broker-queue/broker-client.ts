import { readFile } from "node:fs/promises";
import { homedir } from "node:os";

export type BrokerTask = {
  role: string;
  taskId: string;
  taskMd: string;
  workToken?: string;
};

type BrokerConfig = {
  url: string;
  timeout?: number;
  headers?: Record<string, string>;
};

type BrokerFileConfig = {
  url: string;
  username?: string;
  password?: string;
  xProjectId: string;
  timeout?: number;
  headers?: Record<string, string>;
};

type JsonRpcResponse<T> = {
  result?: T;
  error?: { code: number; message: string; data?: unknown };
};

export class BrokerClient {
  private reqId = 1;
  private initialized = false;

  constructor(private readonly config: BrokerConfig) {}

  static async fromDefaultConfig(): Promise<BrokerClient> {
    const home = homedir();
    const globalPath = `${home}/.pi/agent/broker.json`;
    const projectPath = `${process.cwd()}/.pi/agent/broker.json`;

    const globalRaw = await readFile(globalPath, "utf-8");
    const globalCfg = JSON.parse(globalRaw) as BrokerFileConfig;

    let projectOverride: Partial<BrokerFileConfig> = {};
    try {
      const projectRaw = await readFile(projectPath, "utf-8");
      projectOverride = JSON.parse(projectRaw) as Partial<BrokerFileConfig>;
    } catch {
      // no project override file
    }

    const merged: BrokerFileConfig = {
      ...globalCfg,
      ...projectOverride,
      xProjectId: projectOverride.xProjectId ?? globalCfg.xProjectId,
    };

    if (!merged.url || !merged.xProjectId) {
      throw new Error("Invalid broker config. Required: url, xProjectId");
    }

    const headers: Record<string, string> = {
      "x-project-id": merged.xProjectId,
      ...(merged.headers ?? {}),
    };

    if (merged.username && merged.password) {
      const basic = Buffer.from(`${merged.username}:${merged.password}`).toString("base64");
      headers.Authorization = `Basic ${basic}`;
    }

    return new BrokerClient({
      url: merged.url,
      timeout: merged.timeout,
      headers,
    });
  }

  async ensureInitialized(signal?: AbortSignal): Promise<void> {
    if (this.initialized) return;
    await this.call("initialize", {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "pi-broker-extension", version: "0.1.0" },
    }, signal);
    this.initialized = true;
  }

  async listenRole(role: string, mode: "wait" | "poll" = "wait", signal?: AbortSignal) {
    return this.callTool("listen_role", { role, mode }, signal);
  }

  async solveTask(taskId: string, resultMd: string, signal?: AbortSignal): Promise<any>;
  async solveTask(taskId: string, resultMd: string, workToken?: string, signal?: AbortSignal): Promise<any>;
  async solveTask(
    taskId: string,
    resultMd: string,
    workTokenOrSignal?: string | AbortSignal,
    trailingSignal?: AbortSignal,
  ) {
    const workToken = typeof workTokenOrSignal === "string" ? workTokenOrSignal : undefined;
    const signal = typeof workTokenOrSignal === "string"
      ? trailingSignal
      : (workTokenOrSignal ?? trailingSignal);
    return this.callTool(
      "solve_task",
      { task_id: taskId, result_md: resultMd, ...(workToken ? { work_token: workToken } : {}) },
      signal,
    );
  }

  async progressTask(taskId: string, message: string, workToken?: string, signal?: AbortSignal) {
    return this.callTool(
      "progress_task",
      { task_id: taskId, message, ...(workToken ? { work_token: workToken } : {}) },
      signal,
    );
  }

  async listPickedTasks(role: string, signal?: AbortSignal) {
    return this.callTool("list_tasks", { role, status: "picked" }, signal);
  }

  async getTask(taskId: string, signal?: AbortSignal) {
    return this.callTool(
      "get_task",
      { task_id: taskId, include_task_md: true, include_result_md: false },
      signal,
    );
  }

  private async callTool(name: string, args: Record<string, unknown>, signal?: AbortSignal) {
    const result = await this.call<any>("tools/call", { name, arguments: args }, signal);
    const text = result?.content?.[0]?.text;
    if (typeof text === "string") {
      try {
        return JSON.parse(text);
      } catch {
        return { text };
      }
    }
    return result;
  }

  private async call<T>(method: string, params: Record<string, unknown>, signal?: AbortSignal): Promise<T> {
    const timeoutMs = Math.max(Number(this.config.timeout ?? 3600000), 3600000);
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), timeoutMs);

    const onAbort = () => ctrl.abort();
    if (signal?.aborted) ctrl.abort();
    signal?.addEventListener("abort", onAbort);

    try {
      const resp = await fetch(this.config.url, {
        method: "POST",
        headers: { "Content-Type": "application/json", ...(this.config.headers ?? {}) },
        body: JSON.stringify({
          jsonrpc: "2.0",
          id: this.reqId++,
          method,
          params,
        }),
        signal: ctrl.signal,
      });
      const raw = await resp.text();
      if (!resp.ok) {
        throw new Error(`HTTP ${resp.status}: ${raw.slice(0, 400)}`);
      }

      let data: JsonRpcResponse<T>;
      try {
        data = JSON.parse(raw) as JsonRpcResponse<T>;
      } catch {
        throw new Error(`Invalid JSON-RPC response: ${raw.slice(0, 400)}`);
      }

      if (data.error) throw new Error(data.error.message);
      if (typeof data.result === "undefined") throw new Error("JSON-RPC response missing result");
      return data.result as T;
    } finally {
      clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
    }
  }
}

export function extractTask(decoded: any): BrokerTask | null {
  const taskId = decoded?.task_id ?? decoded?.task?.task_id;
  if (!taskId) return null;
  return {
    role: decoded?.role ?? decoded?.task?.role ?? "coder",
    taskId,
    taskMd: decoded?.task_md ?? decoded?.task?.task_md ?? decoded?.task?.description ?? "",
    workToken: decoded?.work_token ?? decoded?.task?.work_token,
  };
}
