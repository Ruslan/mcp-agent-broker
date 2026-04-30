<script>
  import './app.css';
  import { onMount, onDestroy } from 'svelte';
  import { marked } from 'marked';
  import DOMPurify from 'dompurify';

  let currentView = $state('tasks'); // 'tasks' or 'prompts'
  let projects = $state([]);
  let tasks = $state([]);
  let prompts = $state([]);
  let selectedTask = $state(null);
  let selectedPrompt = $state(null);
  let selectedProject = $state('default');
  let filterRole = $state('');
  let filterStatus = $state('');

  let eventSource;

  async function fetchProjects() {
    const res = await fetch('./api/projects');
    projects = await res.json();
  }

  async function fetchTasks() {
    const params = new URLSearchParams({
      project: selectedProject,
      role: filterRole,
      status: filterStatus
    });
    const res = await fetch(`./api/tasks?${params}`);
    tasks = await res.json();
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
    fetchTasks();
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
          fetchTasks();
        }
        if (selectedTask && selectedTask.metadata.task_id === update.task_id) {
          showTask(update.task_id);
        }
      }
    });
  }

  onMount(() => {
    fetchProjects();
    fetchTasks();
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

  function handleKeydown(e, callback) {
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      callback();
    }
  }
</script>

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
            <select bind:value={selectedProject} onchange={fetchTasks}>
              {#each projects as p}
                <option value={p}>{p}</option>
              {/each}
            </select>
          </li>
        </ul>
        <ul>
          <li><input type="text" placeholder="Filter Role" bind:value={filterRole} oninput={fetchTasks} /></li>
          <li>
            <select bind:value={filterStatus} onchange={fetchTasks}>
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
          <div class="status-{task.status}">{task.status}</div>
          <div>{formatDate(task.updated_at)}</div>
          <div><code>{task.task_id.slice(0,8)}...</code></div>
        </div>
      {/each}
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
    <dialog open>
      <article class="modal-content">
        <header>
          <a href="#close" aria-label="Close" class="close" onclick={() => selectedTask = null}></a>
          {selectedTask.metadata.title}
        </header>
        <div class="grid">
          <div><strong>Status:</strong> <span class="status-{selectedTask.metadata.status}">{selectedTask.metadata.status}</span></div>
          <div><strong>Role:</strong> {selectedTask.metadata.role}</div>
          <div><strong>Updated:</strong> {formatDate(selectedTask.metadata.updated_at)}</div>
        </div>
        
        <h5>Task Description</h5>
        <div class="markdown-body">{@html renderMarkdown(selectedTask.task_md)}</div>

        {#if selectedTask.progress && selectedTask.progress.length > 0}
          <h5>Progress Log</h5>
          <div class="progress-log">
            {#each selectedTask.progress as msg}
              <div class="progress-entry">{msg}</div>
            {/each}
          </div>
        {/if}

        {#if selectedTask.result_md}
          <h5>Result</h5>
          <div class="markdown-body">{@html renderMarkdown(selectedTask.result_md)}</div>
        {/if}

        <footer>
          <div class="modal-footer">
            <button class="outline contrast" onclick={() => deleteTask(selectedTask.metadata.task_id)}>Delete Task</button>
            <button class="secondary" onclick={() => selectedTask = null}>Close</button>
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
