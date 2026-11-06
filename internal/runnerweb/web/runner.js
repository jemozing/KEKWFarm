"use strict";

const byID = (id) => document.getElementById(id);
const passwordKey = "kekwfarm-runner-password";
const jobForm = byID("job-form");
const dockerfileInput = byID("dockerfile");
const connectionForm = byID("connection-form");
const connectionFields = byID("connection-fields");
const connectionURL = byID("connection-url");
const connectionToken = byID("connection-token");
const activeStatuses = new Set(["scheduled", "running", "stopping"]);
const pendingActions = new Set();
const formatter = new Intl.DateTimeFormat(undefined, { dateStyle: "short", timeStyle: "medium" });
const connectionStatuses = {
  online: ["Online", "success"],
  connecting: ["Connecting", "active"],
  offline: ["Offline", "error"],
  unauthenticated: ["Unauthenticated", "error"],
  legacy: ["Legacy", ""],
};
let session;
let currentView = "jobs";
let jobs = [];
let jobsSnapshot;
let selectedJob;
let jobsRequest;
let logsRequest;
let connectionRequest;
let connectionSaveRequest;
let connectionURLChanged = false;
let refreshTimer;

function message(id, text = "", tone = "") {
  const element = byID(id);
  element.classList.toggle("error", tone === "error");
  element.classList.toggle("success", tone === "success");
  element.textContent = text;
  element.hidden = !text;
}

function closeLogs() {
  logsRequest?.abort();
  logsRequest = undefined;
  selectedJob = undefined;
  byID("logs").hidden = true;
  byID("logs-title").textContent = "Job output";
  byID("log-output").textContent = "";
  message("logs-message");
}

function showSession(signedIn) {
  byID("login").hidden = signedIn;
  byID("dashboard").hidden = !signedIn;
  byID("logout").hidden = !signedIn;
  byID("header-connection").hidden = !signedIn;
}

function signOut(reason) {
  session?.controller.abort();
  session = undefined;
  jobsRequest?.abort();
  jobsRequest = undefined;
  clearTimeout(refreshTimer);
  closeLogs();
  clearConnection();
  renderConnectionIndicator("connecting");
  sessionStorage.removeItem(passwordKey);
  jobs = [];
  jobsSnapshot = undefined;
  pendingActions.clear();
  byID("jobs-body").replaceChildren();
  byID("jobs-count").textContent = "";
  byID("job-fields").disabled = false;
  byID("refresh").disabled = false;
  jobForm.reset();
  for (const id of ["schedule-message", "jobs-load-message", "jobs-message"]) message(id);
  showSession(false);
  message("login-message", reason, "error");
  byID("password").value = "";
  byID("password").focus();
}

async function api(path, { method = "GET", body, json, signal, credential = session?.password } = {}) {
  const current = session;
  const headers = { Accept: "application/json", Authorization: credential };
  if (json !== undefined) {
    headers["Content-Type"] = "application/json";
    body = JSON.stringify(json);
  }
  const response = await fetch(`/api/${path}`, {
    method, body, headers, signal: signal || current?.controller.signal, cache: "no-store",
  });
  if (response.status === 401 || response.status === 403) {
    if (path === "login") throw new Error("Incorrect runner password.");
    if (session === current) signOut("Sign in again to continue.");
    throw new Error("Access denied.");
  }
  if (response.status === 204) return undefined;
  const contentType = response.headers.get("Content-Type") || "";
  if (contentType.includes("application/json")) {
    const data = await response.json();
    if (response.ok) return data;
    throw new Error(data?.error || data?.message || `Request failed (${response.status}).`);
  }
  if (response.ok) return undefined;
  const detail = contentType.includes("text/plain") ? (await response.text()).trim() : "";
  throw new Error(detail || `Request failed (${response.status}).`);
}

async function signIn(candidate) {
  const button = byID("login-form").querySelector("button");
  if (button.disabled) return;
  button.disabled = true;
  message("login-message");
  try {
    await api("login", { method: "POST", json: { password: candidate }, credential: candidate });
    session = { password: candidate, controller: new AbortController() };
    sessionStorage.setItem(passwordKey, candidate);
    byID("password").value = "";
    showSession(true);
    await refreshView();
  } catch (error) {
    signOut(error.message);
  } finally {
    button.disabled = false;
  }
}

function addText(parent, tag, text, className) {
  const item = document.createElement(tag);
  item.textContent = text;
  if (className) item.className = className;
  parent.append(item);
  return item;
}

function addButton(parent, label, action) {
  const button = addText(parent, "button", label, "secondary");
  button.type = "button";
  button.addEventListener("click", action);
  return button;
}

function formatTime(raw) {
  const timestamp = Date.parse(raw);
  return Number.isNaN(timestamp) ? "—" : formatter.format(timestamp);
}

function renderJobs() {
  const rows = document.createDocumentFragment();
  for (const job of jobs) {
    const row = document.createElement("tr");
    const name = row.insertCell();
    addText(name, "div", job.name, "job-name");
    addText(name, "div", job.interpreter, "meta muted");
    if (job.build_image) addText(name, "div", "Dockerfile build", "meta muted");
    if (job.image) addText(name, "div", job.image, "meta muted");

    const schedule = row.insertCell();
    const timestamp = job.status === "scheduled" && job.next_at ? job.next_at : job.start_at;
    addText(schedule, "time", formatTime(timestamp)).dateTime = timestamp || "";
    addText(schedule, "div", job.interval_seconds ? `Every ${job.interval_seconds}s` : "Once", "meta muted");

    const limits = row.insertCell();
    addText(limits, "div", `${job.timeout_seconds}s per target`);
    addText(limits, "div", `${job.parallelism} parallel`, "meta muted");

    const status = row.insertCell();
    const active = activeStatuses.has(job.status);
    const tone = active ? "active" : { completed: "success", failed: "error" }[job.status] || "";
    addText(status, "span", job.status, `status ${tone}`);
    if (job.last_error) addText(status, "p", job.last_error, "job-error meta");

    const id = String(job.id);
    const actions = addText(row.insertCell(), "div", "", "actions");
    addButton(actions, "Logs", () => openLogs(job));
    const label = active ? "Stop" : "Delete";
    const button = addButton(actions, label, () => changeJob(id, label, button));
    button.disabled = job.status === "stopping" || pendingActions.has(id);
    rows.append(row);
  }
  if (!jobs.length) {
    const row = document.createElement("tr");
    const empty = addText(row, "td", "No jobs scheduled.", "empty");
    empty.colSpan = 5;
    rows.append(row);
  }
  byID("jobs-body").replaceChildren(rows);
  byID("jobs-count").textContent = `${jobs.length} ${jobs.length === 1 ? "job" : "jobs"}`;
}

async function refreshJobs() {
  if (!session || currentView !== "jobs") return;
  clearTimeout(refreshTimer);
  jobsRequest?.abort();
  const request = new AbortController();
  jobsRequest = request;
  void refreshConnection();
  byID("refresh").disabled = true;
  if (jobsSnapshot === undefined) message("jobs-load-message", "Loading jobs…");
  try {
    const result = (await api("jobs", { signal: request.signal })) || [];
    if (request.signal.aborted) return;
    if (selectedJob && !result.some((job) => String(job.id) === selectedJob)) closeLogs();
    const snapshot = JSON.stringify(result);
    if (snapshot !== jobsSnapshot) {
      jobs = result;
      jobsSnapshot = snapshot;
      renderJobs();
    }
    message("jobs-load-message");
  } catch (error) {
    if (!request.signal.aborted) message("jobs-load-message", error.message, "error");
  } finally {
    if (jobsRequest === request) {
      jobsRequest = undefined;
      byID("refresh").disabled = false;
      void loadLogs();
      scheduleRefresh(refreshJobs);
    }
  }
}

function scheduleRefresh(refresh) {
  if (!document.hidden) refreshTimer = setTimeout(refresh, 3000);
}

function clearConnection() {
  connectionRequest?.abort();
  connectionSaveRequest?.abort();
  connectionRequest = undefined;
  connectionSaveRequest = undefined;
  connectionURLChanged = false;
  connectionForm.reset();
  connectionFields.disabled = true;
  const status = byID("connection-status");
  status.className = "status";
  status.textContent = "—";
  for (const id of ["connection-endpoint", "connection-last-seen", "connection-token-state"]) byID(id).textContent = "—";
  for (const id of ["connection-error", "connection-read-message", "connection-save-message"]) message(id);
}

function renderConnectionIndicator(state) {
  const [label, tone] = connectionStatuses[state] || ["Unknown", ""];
  const status = byID("header-connection-status");
  if (status.textContent === label) return;
  status.className = tone ? `status ${tone}` : "status";
  status.textContent = label;
  byID("header-connection").setAttribute("aria-label", `Farm connection: ${label}`);
}

function renderConnection(config) {
  renderConnectionIndicator(config.status);
  const [label, tone] = connectionStatuses[config.status] || ["Unknown", ""];
  const status = byID("connection-status");
  status.className = tone ? `status ${tone}` : "status";
  if (status.textContent !== label) status.textContent = label;
  byID("connection-endpoint").textContent = config.url || "—";
  const lastSeen = Date.parse(config.last_seen);
  byID("connection-last-seen").textContent = Number.isNaN(lastSeen) ? "Never" : new Date(lastSeen).toISOString().replace(/\.000Z$/, "Z");
  byID("connection-token-state").textContent = config.token_configured ? "Configured" : "Missing";
  const detail = config.error || "";
  if (byID("connection-error").textContent !== detail) message("connection-error", detail, "error");
  if (!connectionURLChanged) connectionURL.value = config.url || "";
}

async function refreshConnection() {
  if (!session || connectionSaveRequest) return;
  const detailed = currentView === "connection";
  connectionRequest?.abort();
  const request = new AbortController();
  connectionRequest = request;
  if (detailed) {
    clearTimeout(refreshTimer);
    byID("refresh").disabled = true;
    if (connectionFields.disabled) message("connection-read-message", "Loading connection…");
  }
  try {
    const config = await api("connection", { signal: request.signal });
    if (request.signal.aborted) return;
    if (detailed) {
      renderConnection(config);
      connectionFields.disabled = false;
      message("connection-read-message");
    } else {
      renderConnectionIndicator(config.status);
    }
  } catch (error) {
    if (!request.signal.aborted) {
      renderConnectionIndicator("unknown");
      if (detailed) message("connection-read-message", error.message, "error");
    }
  } finally {
    if (connectionRequest === request) {
      connectionRequest = undefined;
      if (detailed) {
        byID("refresh").disabled = false;
        scheduleRefresh(refreshConnection);
      }
    }
  }
}

async function refreshView() {
  if (!session) return;
  const view = window.location.hash === "#connection" ? "connection" : "jobs";
  if (view !== currentView) {
    clearTimeout(refreshTimer);
    if (view === "connection") {
      jobsRequest?.abort();
      jobsRequest = undefined;
      closeLogs();
    } else {
      clearConnection();
    }
    currentView = view;
  }
  const connection = view === "connection";
  byID("schedule").hidden = connection;
  byID("job-list").hidden = connection;
  byID("connection-view").hidden = !connection;
  byID("page-title").textContent = connection ? "Connection" : "Runner jobs";
  for (const link of byID("runner-navigation").querySelectorAll("a")) {
    if (link.hash === `#${view}`) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  if (connection) await refreshConnection();
  else await refreshJobs();
}

function openLogs(job) {
  closeLogs();
  selectedJob = String(job.id);
  byID("logs-title").textContent = `Output for ${job.name}`;
  byID("logs").hidden = false;
  message("logs-message", "Loading output…");
  byID("logs").scrollIntoView({ block: "nearest" });
  void loadLogs();
}

async function loadLogs() {
  if (!selectedJob || logsRequest) return;
  const request = new AbortController();
  logsRequest = request;
  try {
    const { lines } = await api(`jobs/${encodeURIComponent(selectedJob)}/logs`, { signal: request.signal });
    if (request.signal.aborted) return;
    const output = byID("log-output");
    const text = lines?.length ? lines.join("\n") : "No output yet.";
    if (text !== output.textContent) {
      const atEnd = output.scrollHeight - output.scrollTop <= output.clientHeight + 12;
      output.textContent = text;
      if (atEnd) output.scrollTop = output.scrollHeight;
    }
    message("logs-message");
  } catch (error) {
    if (!request.signal.aborted) message("logs-message", error.message, "error");
  } finally {
    if (logsRequest === request) logsRequest = undefined;
  }
}

async function changeJob(id, label, button) {
  if (!session || pendingActions.has(id)) return;
  const current = session;
  const stop = label === "Stop";
  pendingActions.add(id);
  button.disabled = true;
  message("jobs-message", stop ? "Stopping job…" : "Deleting job…");
  try {
    await api(`jobs/${encodeURIComponent(id)}${stop ? "/stop" : ""}`, { method: stop ? "POST" : "DELETE" });
    if (session !== current) return;
    message("jobs-message", stop ? "Stop requested." : "Job deleted.", "success");
    if (!stop && selectedJob === id) closeLogs();
    await refreshJobs();
  } catch (error) {
    if (session === current) message("jobs-message", error.message, "error");
  } finally {
    if (session === current) {
      pendingActions.delete(id);
      renderJobs();
    }
  }
}

byID("login-form").addEventListener("submit", (event) => {
  event.preventDefault();
  void signIn(byID("password").value);
});
byID("logout").addEventListener("click", () => signOut());
byID("refresh").addEventListener("click", () => refreshView());
byID("close-logs").addEventListener("click", closeLogs);
window.addEventListener("hashchange", () => refreshView());
window.addEventListener("pagehide", () => { connectionToken.value = ""; });
document.addEventListener("visibilitychange", () => {
  if (document.hidden) clearTimeout(refreshTimer);
  else if (session) void refreshView();
});

connectionURL.addEventListener("input", () => { connectionURLChanged = true; });
connectionForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!session || currentView !== "connection" || connectionFields.disabled) return;
  const url = connectionURL.value.trim();
  try {
    const parsed = new URL(url.includes("://") ? url : `http://${url}`);
    if (!["http:", "https:"].includes(parsed.protocol) || parsed.username || parsed.password || parsed.search || parsed.hash) throw new Error();
  } catch {
    message("connection-save-message", "Enter an HTTP(S) farm URL without credentials, query, or fragment.", "error");
    return;
  }

  const token = connectionToken.value.trim();
  connectionToken.value = "";
  clearTimeout(refreshTimer);
  connectionRequest?.abort();
  connectionRequest = undefined;
  const request = new AbortController();
  connectionSaveRequest = request;
  connectionFields.disabled = true;
  byID("refresh").disabled = true;
  message("connection-save-message", "Saving…");
  try {
    const config = await api("connection", { method: "PUT", json: token ? { url, token } : { url }, signal: request.signal });
    if (request.signal.aborted) return;
    connectionURLChanged = false;
    renderConnection(config);
    message("connection-read-message");
    message("connection-save-message", "Connection saved.", "success");
  } catch (error) {
    if (!request.signal.aborted) message("connection-save-message", error.message, "error");
  } finally {
    if (connectionSaveRequest === request) {
      connectionSaveRequest = undefined;
      connectionFields.disabled = false;
      byID("refresh").disabled = false;
      scheduleRefresh(refreshConnection);
    }
  }
});

function updateImageMode() {
  const build = dockerfileInput.files.length > 0;
  byID("prebuilt-image").hidden = build;
  byID("image").disabled = build;
  byID("remove-dockerfile").hidden = !build;
}

dockerfileInput.addEventListener("change", updateImageMode);
byID("remove-dockerfile").addEventListener("click", () => {
  dockerfileInput.value = "";
  updateImageMode();
  dockerfileInput.focus();
});
jobForm.addEventListener("reset", () => queueMicrotask(updateImageMode));

jobForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const fields = byID("job-fields");
  if (!session || fields.disabled) return;
  const current = session;
  const body = new FormData(jobForm);
  const file = body.get("file");
  if (!file?.name || file.size === 0) {
    message("schedule-message", "Choose a nonempty exploit file.", "error");
    return;
  }
  if (file.size > 10 * 1024 * 1024) {
    message("schedule-message", "Choose a file up to 10 MiB.", "error");
    return;
  }
  const name = body.get("name").trim();
  if (new TextEncoder().encode(name || file.name).length > 160) {
    message("schedule-message", "Enter a job name up to 160 bytes.", "error");
    return;
  }
  if (name) body.set("name", name);
  else body.delete("name");
  const dockerfile = dockerfileInput.files[0];
  if (dockerfile && (dockerfile.size === 0 || dockerfile.size > 256 * 1024)) {
    message("schedule-message", "Choose a nonempty Dockerfile up to 256 KiB.", "error");
    return;
  }
  if (!dockerfile) body.delete("dockerfile");
  const image = body.get("image")?.trim();
  if (!dockerfile && image) body.set("image", image);
  else body.delete("image");
  const start = body.get("start_at");
  if (start) {
    const date = new Date(start);
    if (Number.isNaN(date.valueOf())) {
      message("schedule-message", "Enter a valid start time.", "error");
      return;
    }
    body.set("start_at", date.toISOString());
  } else {
    body.delete("start_at");
  }

  fields.disabled = true;
  message("schedule-message", "Uploading…");
  try {
    await api("jobs", { method: "POST", body });
    if (session !== current) return;
    jobForm.reset();
    message("schedule-message", "Job scheduled.", "success");
    await refreshJobs();
  } catch (error) {
    if (session === current) message("schedule-message", error.message, "error");
  } finally {
    if (session === current) fields.disabled = false;
  }
});

const savedPassword = sessionStorage.getItem(passwordKey);
if (savedPassword) void signIn(savedPassword);
