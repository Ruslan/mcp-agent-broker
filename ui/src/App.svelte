<script>
  import './app.css';
  import { onMount, onDestroy } from 'svelte';
  import { marked } from 'marked';
  import DOMPurify from 'dompurify';

  const PAGE_SIZE = 50;
  const PROJECT_PARAM = 'project';

  let currentView = $state('tasks');
  let projects = $state([]);
  let tasks = $state([]);
  let totalTasks = $state(0);
  let prompts = $state([]);
  let selectedTask = $state(null);
  let selectedPrompt = $state(null);
  let selectedProject = $state('default');
  let filterRole = $state('');
  let filterStatus = $state('');

  let eventSource;

  function readProjectFromURL() {
    const project = new URL(window.location.href).searchParams.get(PROJECT_PARAM)?.trim();
    return project || 'default';
  }

  function writeProjectToURL() {
    const url = new URL(window.location.href);
    url.searchParams.set(PROJECT_PARAM, selectedProject);
    history.replaceState({}, '', url);
  }

  function selectProject() {
    selectedTask = null;
    writeProjectToURL();
    fetchTasks(true);
  }

  async function fetchProjects() {
    const res = await fetch('./api/projects');
    projects = await res.json();
  }

  async function fetchTasks(reset = true) {
    const params = new URLSearchParams({
      project: selectedProject,
      role: filterRole,
      status: filterStatus,
      limit: String(PAGE_SIZE),
      offset: reset ? '0' : String(tasks.length)
    });
    const res = await fetch(`./api/tasks?${params}`);
    const data = await res.json();
    if (reset) {
      tasks = data.tasks;
    } else {
      tasks = [...tasks, ...data.tasks];
    }
    totalTasks = data.total;
  }

  async function fetchPrompts() {
    const res = await fetch('./api/prompts');
    prompts = await res.json();
  }

  async function showTask(taskID) {
    const res = await fetch(`./api/tasks/${taskID}?project=${selectedProject}`);
    selectedTask = await res.json();
  }

  async function deleteTask(taskID) {
    if (!confirm('Are you sure you want to delete this task?')) return;
    const res = await fetch(`./api/tasks/${taskID}?project=${selectedProject}`, { method: 'DELETE' });
    if (!res.ok) {
      alert(`Failed to delete task: ${await res.text()}`);
      return;
    }
    selectedTask = null;
    fetchTasks(true);
  }

  async function updateStatus(taskID, newStatus) {
    if (!newStatus) return;
    if (!confirm(`Change status to "${newStatus}"?`)) return;
    const res = await fetch(`./api/tasks/${taskID}?project=${selectedProject}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ status: newStatus })
    });
    if (!res.ok) {
      alert(`Failed to update status: ${await res.text()}`);
      return;
    }
    showTask(taskID);
    fetchTasks(true);
  }

  async function showPrompt(name) {
    const res = await fetch(`./api/prompts/${name}`);
    selectedPrompt = await res.json();
  }

  function setupSSE() {
    if (eventSource) eventSource.close();
    eventSource = new EventSource('./events');
    eventSource.addEventListener('task_update', (e) => {
      const update = JSON.parse(e.data);
      if (update.project_id === selectedProject) {
        if (currentView === 'tasks') {
          fetchTasks(true);
        }
        if (selectedTask && selectedTask.metadata.task_id === update.task_id) {
          showTask(update.task_id);
        }
      }
    });
  }

  onMount(() => {
    selectedProject = readProjectFromURL();
    writeProjectToURL();
    fetchProjects();
    fetchTasks(true);
    fetchPrompts();
    setupSSE();
  });

  onDestroy(() => {
    if (eventSource) eventSource.close();
  });

  function formatDate(d) {
    return new Date(d).toLocaleTimeString();
  }

  function renderMarkdown(md) {
    if (!md) return '';
    return DOMPurify.sanitize(marked.parse(md));
  }

  function closeTaskDialog() {
    selectedTask = null;
  }

  function handleWindowKeydown(e) {
    if (e.key === 'Escape' && selectedTask) {
      e.preventDefault();
      closeTaskDialog();
    }
  }

  function handleTaskDialogClick(e) {
    if (e.target === e.currentTarget) {
      closeTaskDialog();
    }
  }

  function handleKeydown(e, callback) {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      callback();
    }
  }

  async function copyTaskID(e, taskID) {
    e.preventDefault();
    e.stopPropagation();
    try {
      await navigator.clipboard.writeText(taskID);
    } catch {
      // ignore clipboard errors in unsupported contexts
    }
  }
</script>

<svelte:window onkeydown={handleWindowKeydown} />

<main class="container">
  <header>
    <div style="display: flex; align-items: center;">
      <h1>Agent Broker Admin</h1>
      <div class="view-tabs" style="margin-left: 2rem;">
        <button class={currentView === 'tasks' ? 'active' : 'outline'} onclick={() => currentView = 'tasks'}>Tasks</button>
        <button class={currentView === 'prompts' ? 'active' : 'outline'} onclick={() => currentView = 'prompts'}>Prompts</button>
      </div>
    </div>
    <nav>
      {#if currentView === 'tasks'}
        <ul>
          <li>
            <select bind:value={selectedProject} onchange={selectProject}>
              {#each projects as p}
                <option value={p}>{p}</option>
              {/each}
            </select>
          </li>
        </ul>
        <ul>
          <li><input type="text" placeholder="Filter Role" bind:value={filterRole} oninput={() => fetchTasks(true)} /></li>
          <li>
            <select bind:value={filterStatus} onchange={() => fetchTasks(true)}>
              <option value="">All Statuses</option>
              <option value="queued">Queued</option>
              <option value="picked">Picked</option>
              <option value="solved">Solved</option>
            </select>
          </li>
        </ul>
      {:else}
        <ul>
          <li><button class="outline" onclick={fetchPrompts}>Refresh Prompts</button></li>
        </ul>
      {/if}
    </nav>
  </header>

  {#if currentView === 'tasks'}
    <section>
      <div class="grid-tasks header">
        <div>Title</div>
        <div>Role</div>
        <div>Status</div>
        <div>Created</div>
        <div>Updated</div>
        <div>Task ID</div>
      </div>
      {#each tasks as task}
        <div class="grid-tasks task-row"
             role="button"
             tabindex="0"
             onclick={() => showTask(task.task_id)}
             onkeydown={(e) => handleKeydown(e, () => showTask(task.task_id))}>
          <div><strong>{task.title}</strong></div>
          <div><kbd>{task.role}</kbd></div>
          <div class="status-{task.status}">
            {task.status}
            {#if task.status === 'solved'}
              <span class="row-view-count row-view-{task.result_view_count === 0 ? '0' : task.result_view_count === 1 ? '1' : 'many'}">
                {task.result_view_count === 0 ? '0' : task.result_view_count === 1 ? '1' : '1+'}
              </span>
            {/if}
          </div>
          <div>{formatDate(task.created_at)}</div>
          <div>{formatDate(task.updated_at)}</div>
          <div class="task-id-cell">
            <code>{task.task_id.slice(0, 8)}...</code>
            <button class="copy-id-btn" onclick={(e) => copyTaskID(e, task.task_id)} aria-label="Copy full task id">Copy</button>
          </div>
        </div>
      {/each}
      {#if tasks.length < totalTasks}
        <div class="load-more">
          <button class="outline" onclick={() => fetchTasks(false)}>
            Load more ({tasks.length} of {totalTasks})
          </button>
        </div>
      {/if}
    </section>
  {:else}
    <section>
      <div class="grid-tasks header" style="grid-template-columns: 1fr 2fr 3fr;">
        <div>Name</div>
        <div>Title</div>
        <div>Description</div>
      </div>
      {#each prompts as prompt}
        <div class="grid-tasks task-row"
             style="grid-template-columns: 1fr 2fr 3fr;"
             role="button"
             tabindex="0"
             onclick={() => showPrompt(prompt.name)}
             onkeydown={(e) => handleKeydown(e, () => showPrompt(prompt.name))}>
          <div><strong>{prompt.name}</strong></div>
          <div>{prompt.title || ''}</div>
          <div class="status-queued">{prompt.description || ''}</div>
        </div>
      {/each}
    </section>
  {/if}

  {#if selectedTask}
    <dialog open onclick={handleTaskDialogClick}>
      <article class="modal-content">
        <header>
          <a href="#close" aria-label="Close" class="close" onclick={(e) => { e.preventDefault(); closeTaskDialog(); }}></a>
          {selectedTask.metadata.title}
          <div class="task-id-full-line"><code>{selectedTask.metadata.task_id}</code></div>
        </header>
        <div class="task-modal-meta">
          <div class="meta-item"><span>Status</span><strong class="status-{selectedTask.metadata.status}">{selectedTask.metadata.status}</strong></div>
          <div class="meta-item"><span>Role</span><strong>{selectedTask.metadata.role}</strong></div>
          <div class="meta-item"><span>Created</span><strong>{formatDate(selectedTask.metadata.created_at)}</strong></div>
          <div class="meta-item"><span>Updated</span><strong>{formatDate(selectedTask.metadata.updated_at)}</strong></div>
          <div class="meta-item">
            <span>Views</span>
            <strong class="view-count {selectedTask.metadata.result_view_count === 0 && selectedTask.metadata.status === 'solved' ? 'view-zero-solved' : selectedTask.metadata.result_view_count >= 1 ? 'view-positive' : 'view-zero'}">
              {selectedTask.metadata.result_view_count === 0 ? '0' : selectedTask.metadata.result_view_count === 1 ? '1' : '1+'}
            </strong>
          </div>
          <div class="status-edit">
            <label>
              Change status
              <select onchange={(e) => updateStatus(selectedTask.metadata.task_id, e.target.value)}>
                <option value="">--</option>
                {#if selectedTask.metadata.status !== 'queued'}
                  <option value="queued">queued</option>
                {/if}
                {#if selectedTask.metadata.status !== 'solved'}
                  <option value="solved">solved</option>
                {/if}
              </select>
            </label>
          </div>
        </div>
        <div class="task-modal-grid">
          <section class="task-modal-panel">
            <h5>Task</h5>
            <div class="markdown-body">{@html renderMarkdown(selectedTask.task_md)}</div>

            {#if selectedTask.progress && selectedTask.progress.length > 0}
              <h5 class="progress-heading">Progress Log</h5>
              <div class="progress-log">
                {#each selectedTask.progress as msg}
                  <div class="progress-entry">{msg}</div>
                {/each}
              </div>
            {/if}
          </section>

          <section class="task-modal-panel result-panel">
            <h5>Result</h5>
            {#if selectedTask.result_md}
              <div class="markdown-body">{@html renderMarkdown(selectedTask.result_md)}</div>
            {:else}
              <div class="empty-result">No result yet.</div>
            {/if}
          </section>
        </div>

        <footer>
          <div class="modal-footer">
            <button class="outline contrast" onclick={() => deleteTask(selectedTask.metadata.task_id)}>Delete Task</button>
            <button class="secondary" onclick={closeTaskDialog}>Close</button>
          </div>
        </footer>
      </article>
    </dialog>
  {/if}

  {#if selectedPrompt}
    <dialog open>
      <article class="modal-content">
        <header>
          <a href="#close" aria-label="Close" class="close" onclick={() => selectedPrompt = null}></a>
          Prompt: {selectedPrompt.metadata.name}
        </header>

        <h5>Metadata</h5>
        <table class="meta-table">
          <thead>
            <tr>
              <th>Property</th>
              <th>Value</th>
            </tr>
          </thead>
          <tbody>
            <tr>
              <td>Title</td>
              <td>{selectedPrompt.metadata.title || selectedPrompt.metadata.name}</td>
            </tr>
            <tr>
              <td>Description</td>
              <td>{selectedPrompt.metadata.description || 'N/A'}</td>
            </tr>
          </tbody>
        </table>

        {#if selectedPrompt.metadata.arguments && selectedPrompt.metadata.arguments.length > 0}
          <h5>Arguments</h5>
          <table class="meta-table">
            <thead>
              <tr>
                <th>Argument</th>
                <th>Description</th>
                <th>Required</th>
              </tr>
            </thead>
            <tbody>
              {#each selectedPrompt.metadata.arguments as arg}
                <tr>
                  <td><code>{arg.name}</code></td>
                  <td>{arg.description || ''}</td>
                  <td>{arg.required ? 'Yes' : 'No'}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        {/if}

        <h5>Template Body</h5>
        <div class="markdown-body">{@html renderMarkdown(selectedPrompt.body)}</div>

        <footer>
          <div class="modal-footer" style="justify-content: flex-end;">
            <button class="secondary" onclick={() => selectedPrompt = null}>Close</button>
          </div>
        </footer>
      </article>
    </dialog>
  {/if}
</main>
