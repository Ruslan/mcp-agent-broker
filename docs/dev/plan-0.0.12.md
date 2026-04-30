# Plan 0.0.12: Persistence & Admin Polish

## Goal

Enhance the admin experience with task deletion capabilities, persistent progress logging, and a premium dark-themed interface. Ensure the system is robust for fresh deployments.

## Changes

### 1. Task Deletion
- **Backend**: Added `DeleteTask(projectID, taskID)` to `Store` and `Broker`.
- **API**: Added `DELETE /admin/api/tasks/:id` endpoint.
- **UI**: Added a "Delete Task" button in the task detail modal with a confirmation prompt.

### 2. Progress Persistence
- **Database**: Created `task_progress` table with `ON DELETE CASCADE` linkage to tasks.
- **Broker**: Updated `ReportProgress` to persist messages to SQLite in addition to in-memory channel delivery.
- **API**: Included `progress` array in the `GET /admin/api/tasks/:id` response.
- **UI**: Added a "Progress Log" section to the task detail modal to show the full execution history.

### 3. UI Polish & Dark Theme
- **Theme**: Forced a premium dark theme using Pico CSS `data-theme="dark"`.
- **Aesthetics**: 
    - Implemented a Deep Slate (`#0f172a`) palette with Indigo accents.
    - Added glassmorphism effects for the sticky header.
    - Enhanced status badges with glowing emerald/amber colors.
    - Improved typography using the Inter font family.
- **Layout**: Sticky header, responsive container, and better spacing for task rows.

### 4. Reliability & DX
- **Embedding**: Fixed a bug where the embedded filesystem showed a "dist" folder listing. Now correctly uses `fs.Sub` to serve the UI root.
- **Automation**: Added `npm ci` to the `make ui-build` target to ensure fresh clones can be built immediately.
- **Initialization**: Added automatic creation of the `data/` directory if it doesn't exist on startup.

## Files Modified

- `agent-broker/store.go`: Schema updates and progress persistence logic.
- `agent-broker/broker.go`: Persistence integration and deletion logic.
- `agent-broker/admin.go`: New API endpoints and response shapes.
- `agent-broker/main.go`: FS embedding fix, data dir creation, and read timeout correction.
- `ui/src/App.svelte`: Progress log display and task deletion UI.
- `ui/src/app.css`: Full dark theme styling.
- `ui/index.html`: Dark theme activation and metadata.
- `Makefile`: Build process improvements.
- `README.md`: Updated documentation for prerequisites and endpoints.
