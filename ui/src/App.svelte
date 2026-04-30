<script>
  import './app.css';
  import { onMount, onDestroy } from 'svelte';

  let projects = $state([]);
  let tasks = $state([]);
  let selectedTask = $state(null);
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

  async function showTask(taskID) {
    const res = await fetch(`./api/tasks/${taskID}?project=${selectedProject}`);
    selectedTask = await res.json();
  }

  function setupSSE() {
    if (eventSource) eventSource.close();
    eventSource = new EventSource('./events');
    eventSource.addEventListener('task_update', (e) => {
      const update = JSON.parse(e.data);
      if (update.project_id === selectedProject) {
        // Refresh task list
        fetchTasks();
        // Refresh detail if open
        if (selectedTask && selectedTask.metadata.task_id === update.task_id) {
          showTask(update.task_id);
        }
      }
    });
  }

  onMount(() => {
    fetchProjects();
    fetchTasks();
    setupSSE();
  });

  onDestroy(() => {
    if (eventSource) eventSource.close();
  });

  function formatDate(d) {
    return new Date(d).toLocaleTimeString();
  }
</script>

<main class="container">
  <header>
    <h1>Agent Broker Admin</h1>
    <nav>
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
    </nav>
  </header>

  <section>
    <div class="grid-tasks header">
      <div>Title</div>
      <div>Role</div>
      <div>Status</div>
      <div>Updated</div>
      <div>Task ID</div>
    </div>
    {#each tasks as task}
      <div class="grid-tasks task-row" onclick={() => showTask(task.task_id)}>
        <div><strong>{task.title}</strong></div>
        <div><kbd>{task.role}</kbd></div>
        <div class="status-{task.status}">{task.status}</div>
        <div>{formatDate(task.updated_at)}</div>
        <div><code>{task.task_id.slice(0,8)}...</code></div>
      </div>
    {/each}
  </section>

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
        <pre>{selectedTask.task_md}</pre>

        {#if selectedTask.result_md}
          <h5>Result</h5>
          <pre>{selectedTask.result_md}</pre>
        {/if}

        <footer>
          <button class="secondary" onclick={() => selectedTask = null}>Close</button>
        </footer>
      </article>
    </dialog>
  {/if}
</main>
