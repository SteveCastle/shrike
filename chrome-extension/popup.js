// Shrike Chrome Extension
const API_BASE = "http://localhost:8090";

// State
let eventSource = null;
let jobs = [];
let isConnecting = false;

// DOM Elements
const elements = {
  serverStatus: null,
  command: null,
  args: null,
  url: null,
  createBtn: null,
  feedback: null,
  jobsList: null,
  refreshBtn: null,
  clearAllBtn: null,
};

// Initialize on DOM load
document.addEventListener("DOMContentLoaded", init);

async function init() {
  // Cache DOM elements
  elements.serverStatus = document.getElementById("serverStatus");
  elements.command = document.getElementById("command");
  elements.args = document.getElementById("args");
  elements.url = document.getElementById("url");
  elements.createBtn = document.getElementById("createBtn");
  elements.feedback = document.getElementById("feedback");
  elements.jobsList = document.getElementById("jobsList");
  elements.refreshBtn = document.getElementById("refreshBtn");
  elements.clearAllBtn = document.getElementById("clearAllBtn");

  // Load saved preferences
  loadPreferences();

  // Get current tab URL
  await populateCurrentUrl();

  // Set up event listeners
  elements.createBtn.addEventListener("click", createTask);
  elements.refreshBtn.addEventListener("click", refreshJobs);
  elements.clearAllBtn.addEventListener("click", clearAllJobs);
  elements.command.addEventListener("change", savePreferences);
  elements.args.addEventListener("input", debounce(savePreferences, 500));

  // Allow Enter key to submit
  elements.args.addEventListener("keypress", (e) => {
    if (e.key === "Enter") createTask();
  });
  elements.url.addEventListener("keypress", (e) => {
    if (e.key === "Enter") createTask();
  });

  // Set up event delegation for job actions
  elements.jobsList.addEventListener("click", handleJobClick);

  // Connect to SSE and fetch initial jobs
  connectSSE();
  fetchJobs();
}

// Handle clicks on job items using event delegation
function handleJobClick(e) {
  // Find the closest element with a data-action attribute
  const target = e.target.closest("[data-action]");
  if (!target) return;

  const action = target.getAttribute("data-action");
  const id = target.getAttribute("data-id");
  if (!id) return;

  e.preventDefault();

  switch (action) {
    case "cancel":
      cancelJob(id);
      break;
    case "remove":
      removeJobFromServer(id);
      break;
    case "open":
      openJobDetail(id);
      break;
  }
}

// Get current tab URL
async function populateCurrentUrl() {
  try {
    const [tab] = await chrome.tabs.query({
      active: true,
      currentWindow: true,
    });
    if (tab?.url) {
      elements.url.value = tab.url;
    }
  } catch (err) {
    console.error("Failed to get current tab URL:", err);
  }
}

// Save/load preferences
function savePreferences() {
  chrome.storage.local.set({
    lastCommand: elements.command.value,
    lastArgs: elements.args.value,
  });
}

function loadPreferences() {
  chrome.storage.local.get(["lastCommand", "lastArgs"], (result) => {
    if (result.lastCommand) {
      elements.command.value = result.lastCommand;
    }
    if (result.lastArgs) {
      elements.args.value = result.lastArgs;
    }
  });
}

// Create task
async function createTask() {
  const command = elements.command.value;
  const args = elements.args.value.trim();
  const url = elements.url.value.trim();

  if (!url) {
    showFeedback("Please enter a URL or input", "error");
    return;
  }

  // Build the input string: command [args] url
  let input = command;
  if (args) {
    input += ` ${args}`;
  }
  input += ` ${url}`;

  // Disable button and show loading
  setLoading(true);
  hideFeedback();

  try {
    const response = await fetch(`${API_BASE}/create`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ input }),
    });

    if (!response.ok) {
      const text = await response.text();
      throw new Error(text || `HTTP ${response.status}`);
    }

    const data = await response.json();
    showFeedback(`Task created: ${data.id.slice(0, 8)}...`, "success");

    // Refresh jobs list
    fetchJobs();
  } catch (err) {
    console.error("Create task error:", err);
    showFeedback(`Failed: ${err.message}`, "error");
  } finally {
    setLoading(false);
  }
}

// Fetch current jobs
async function fetchJobs() {
  try {
    const response = await fetch(`${API_BASE}/jobs/list`);
    if (!response.ok) throw new Error("Failed to fetch jobs");

    const fetchedJobs = await response.json();
    updateServerStatus(true);

    // Update jobs array with fetched jobs
    if (Array.isArray(fetchedJobs)) {
      // Merge with existing jobs, preferring fetched data
      fetchedJobs.forEach((job) => {
        const existingIndex = jobs.findIndex((j) => j.id === job.id);
        if (existingIndex >= 0) {
          jobs[existingIndex] = job;
        } else {
          jobs.push(job);
        }
      });

      // Sort by created_at descending (newest first)
      jobs.sort((a, b) => new Date(b.created_at) - new Date(a.created_at));

      // Keep only last 20 jobs
      if (jobs.length > 20) {
        jobs = jobs.slice(0, 20);
      }

      renderJobs();
    }
  } catch (err) {
    console.error("Fetch jobs error:", err);
    updateServerStatus(false);
  }
}

// Refresh jobs
function refreshJobs() {
  // Reconnect SSE to get fresh state
  if (eventSource) {
    eventSource.close();
  }
  jobs = [];
  renderJobs();
  connectSSE();
  fetchJobs();
}

// Clear all non-running jobs
async function clearAllJobs() {
  try {
    const response = await fetch(`${API_BASE}/jobs/clear`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
    });

    if (!response.ok) {
      throw new Error("Failed to clear jobs");
    }

    // Refresh the jobs list
    fetchJobs();
  } catch (err) {
    console.error("Clear all jobs error:", err);
    showFeedback("Failed to clear jobs", "error");
  }
}

// SSE Connection
function connectSSE() {
  if (
    isConnecting ||
    (eventSource && eventSource.readyState === EventSource.OPEN)
  ) {
    return;
  }

  isConnecting = true;
  updateServerStatus("connecting");

  try {
    eventSource = new EventSource(`${API_BASE}/stream`);

    eventSource.onopen = () => {
      isConnecting = false;
      updateServerStatus(true);
      console.log("SSE connected");
    };

    eventSource.onerror = (err) => {
      console.error("SSE error:", err);
      isConnecting = false;
      updateServerStatus(false);

      // Close and attempt reconnect after delay
      if (eventSource) {
        eventSource.close();
        eventSource = null;
      }

      setTimeout(connectSSE, 5000);
    };

    // Job creation event
    eventSource.addEventListener("create", (event) => {
      try {
        const data = JSON.parse(event.data);
        if (data.job) {
          addOrUpdateJob(data.job);
        }
      } catch (e) {
        console.error("Parse create event:", e);
      }
    });

    // Job update event
    eventSource.addEventListener("update", (event) => {
      try {
        const data = JSON.parse(event.data);
        if (data.job) {
          addOrUpdateJob(data.job);
        }
      } catch (e) {
        console.error("Parse update event:", e);
      }
    });

    // Job deletion event
    eventSource.addEventListener("delete", (event) => {
      try {
        const data = JSON.parse(event.data);
        if (data.job?.id) {
          removeJob(data.job.id);
        }
      } catch (e) {
        console.error("Parse delete event:", e);
      }
    });
  } catch (err) {
    console.error("SSE connect error:", err);
    isConnecting = false;
    updateServerStatus(false);
  }
}

// Job management
function addOrUpdateJob(job) {
  const existingIndex = jobs.findIndex((j) => j.id === job.id);
  if (existingIndex >= 0) {
    jobs[existingIndex] = job;
  } else {
    jobs.unshift(job);
  }

  // Keep only last 20 jobs in the extension
  if (jobs.length > 20) {
    jobs = jobs.slice(0, 20);
  }

  renderJobs();
}

function removeJob(jobId) {
  jobs = jobs.filter((j) => j.id !== jobId);
  renderJobs();
}

function renderJobs() {
  // Filter to show only active/recent jobs
  const activeJobs = jobs.filter((j) => j.state === 0 || j.state === 1);
  const recentJobs = jobs
    .filter((j) => j.state !== 0 && j.state !== 1)
    .slice(0, 5);
  const displayJobs = [...activeJobs, ...recentJobs].slice(0, 10);

  if (displayJobs.length === 0) {
    elements.jobsList.innerHTML = `
      <div class="jobs-empty">
        <span>No active jobs</span>
      </div>
    `;
    return;
  }

  elements.jobsList.innerHTML = displayJobs
    .map((job) => {
      const statusClass = getStatusClass(job.state);
      const timeAgo = formatTimeAgo(job.created_at);
      const truncatedInput = truncate(job.input || "", 50);
      const isActive = job.state === 0 || job.state === 1;

      return `
      <div class="job-item" data-id="${job.id}">
        <div class="job-status ${statusClass}"></div>
        <div class="job-details" data-action="open" data-id="${
          job.id
        }" style="cursor: pointer;" title="Open in web UI">
          <div class="job-command">${escapeHtml(job.command || "Unknown")}</div>
          <div class="job-input" title="${escapeHtml(
            job.input || ""
          )}">${escapeHtml(truncatedInput)}</div>
          <div class="job-time">${timeAgo}</div>
        </div>
        <div class="job-actions">
          ${
            isActive
              ? `<button class="job-action-btn cancel" data-action="cancel" data-id="${job.id}" title="Cancel">✕</button>`
              : `<button class="job-action-btn remove" data-action="remove" data-id="${job.id}" title="Remove">✕</button>`
          }
        </div>
      </div>
    `;
    })
    .join("");
}

// Cancel a job
async function cancelJob(jobId) {
  try {
    const response = await fetch(`${API_BASE}/job/${jobId}/cancel`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
    });

    if (!response.ok) {
      throw new Error("Failed to cancel job");
    }
  } catch (err) {
    console.error("Cancel job error:", err);
    showFeedback("Failed to cancel job", "error");
  }
}

// Remove a job from the server
async function removeJobFromServer(jobId) {
  try {
    const response = await fetch(`${API_BASE}/job/${jobId}/remove`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
    });

    if (!response.ok) {
      throw new Error("Failed to remove job");
    }

    // Remove from local list
    removeJob(jobId);
  } catch (err) {
    console.error("Remove job error:", err);
    showFeedback("Failed to remove job", "error");
  }
}

// Open job detail in web UI
function openJobDetail(jobId) {
  chrome.tabs.create({ url: `${API_BASE}/job/${jobId}` });
}

// UI Helpers
function updateServerStatus(connected) {
  if (connected === "connecting") {
    elements.serverStatus.className = "status-indicator connecting";
    elements.serverStatus.title = "Connecting...";
  } else if (connected) {
    elements.serverStatus.className = "status-indicator connected";
    elements.serverStatus.title = "Connected to Shrike server";
  } else {
    elements.serverStatus.className = "status-indicator";
    elements.serverStatus.title = "Disconnected from server";
  }
}

function setLoading(loading) {
  elements.createBtn.disabled = loading;
  elements.createBtn.querySelector(".btn-text").style.display = loading
    ? "none"
    : "inline";
  elements.createBtn.querySelector(".btn-loading").style.display = loading
    ? "inline-flex"
    : "none";
}

function showFeedback(message, type) {
  elements.feedback.textContent = message;
  elements.feedback.className = `feedback ${type}`;
  elements.feedback.style.display = "flex";

  // Auto-hide after 5 seconds for success messages
  if (type === "success") {
    setTimeout(hideFeedback, 5000);
  }
}

function hideFeedback() {
  elements.feedback.style.display = "none";
}

// Utility functions
function getStatusClass(state) {
  switch (state) {
    case 0:
      return "pending";
    case 1:
      return "running";
    case 2:
      return "completed";
    case 3:
      return "cancelled";
    case 4:
      return "error";
    default:
      return "pending";
  }
}

function formatTimeAgo(timestamp) {
  if (!timestamp) return "";

  const date = new Date(timestamp);
  const now = new Date();
  const diff = Math.floor((now - date) / 1000);

  if (diff < 60) return "Just now";
  if (diff < 3600) return `${Math.floor(diff / 60)}m ago`;
  if (diff < 86400) return `${Math.floor(diff / 3600)}h ago`;
  return `${Math.floor(diff / 86400)}d ago`;
}

function truncate(str, maxLen) {
  if (str.length <= maxLen) return str;
  return str.slice(0, maxLen - 3) + "...";
}

function escapeHtml(str) {
  const div = document.createElement("div");
  div.textContent = str;
  return div.innerHTML;
}

function debounce(fn, ms) {
  let timeout;
  return (...args) => {
    clearTimeout(timeout);
    timeout = setTimeout(() => fn(...args), ms);
  };
}

// Cleanup on popup close
window.addEventListener("unload", () => {
  if (eventSource) {
    eventSource.close();
  }
});
