"use strict";

const byID = (id) => document.getElementById(id);
const pageSize = 30;
const passwordKey = "duckfarm-password";
const views = { flags: "Flags", teams: "Teams", summary: "Summary", runners: "Runners", checker: "Checker" };
const pageButtons = document.querySelectorAll("[data-page]");
const filtersForm = byID("filters-form");
const runnerForm = byID("runner-form");
const runnerJobStatuses = ["scheduled", "running", "stopping", "completed", "failed", "stopped"];
const checkerForm = byID("checker-form");
const checkerControls = byID("checker-controls");
const checkerProtocol = byID("checker-protocol");
const checkerJSON = byID("checker-json");
const checkerFields = checkerForm.querySelectorAll("[data-checker-field]");
const submissionForm = byID("submission-form");
const submissionControls = byID("submission-controls");
const submissionFields = submissionControls.querySelectorAll("input");
const tcpFields = ["host", "port"];
const httpTokenFields = ["url", "token"];
const httpHostFields = ["url", "host", "port"];
const checkerPresets = {
  "": { fields: [], hint: "The farm keeps collecting flags while submission is disabled." },
  forcad_tcp: { label: "ForcAD · TCP", fields: [...tcpFields, "token"], hint: "Enter the checker host, port, and single-line team token." },
  forcad_http: { label: "ForcAD · HTTP", fields: httpTokenFields, hint: "Enter the complete HTTP(S) submission URL and team token." },
  omctf: {
    label: "OmCTF · Skipper", protocol: "forcad_http", fields: httpTokenFields,
    defaultURL: "http://10.10.10.10/api/v1/flags",
    hint: "Enter the team token from skipper-token.txt. The farm must be able to reach Skipper over the competition VPN.",
  },
  ructf_tcp: { label: "RuCTF · TCP", fields: tcpFields, hint: "Enter the checker host and port." },
  ructf_http: { label: "RuCTF · HTTP", fields: httpTokenFields, hint: "Enter the complete HTTP(S) submission URL and team token." },
  ctfcup_tcp: { label: "CTFCup · TCP", fields: tcpFields, hint: "Enter the checker host and port." },
  faust: { label: "FAUST · TCP", fields: tcpFields, hint: "Enter the checker host and port." },
  ctf01d: { label: "CTF01d · HTTP", fields: ["url", "team_id"], hint: "Enter the complete HTTP(S) submission URL and team ID." },
  ctf_moscow: { label: "CTF.Moscow · HTTPS", fields: httpHostFields, hint: "Use the full URL or enter a host and optional port for HTTPS /api/flag/v1/submit." },
  volgactf: { label: "VolgaCTF · HTTPS", fields: httpHostFields, hint: "Use the full URL or enter a host and optional port for HTTPS /api/flag/v1/submit." },
  volgactf_old: { label: "VolgaCTF / Themis · HTTP", fields: httpHostFields, hint: "Use the full URL or enter a host and optional port for HTTP /api/submit." },
};
for (const [id, preset] of Object.entries(checkerPresets)) {
  if (id) checkerProtocol.add(new Option(`${preset.label} (${preset.protocol ?? id})`, id));
}
let password = "";
let filters = new URLSearchParams();
let page = 1;
let lastPage = 1;
let pendingRequest;
let runnerRequest;
let runnerPoll;
let checkerSaveRequest;
let submissionSaveRequest;
let checkerOptions = {};
let checkerOptionsProtocol = "";

function message(id, text = "", tone = "") {
  const element = byID(id);
  element.textContent = text;
  element.className = tone ? `notice ${tone}` : "notice";
  element.hidden = !text;
}

function updateRefreshButton() {
  byID("refresh").disabled = Boolean(pendingRequest || checkerSaveRequest || submissionSaveRequest);
}

function showSession(active) {
  byID("login").hidden = active;
  for (const id of ["navigation", "logout", "dashboard"]) {
    byID(id).hidden = !active;
  }
}

function signOut(reason) {
  pendingRequest?.abort();
  password = "";
  sessionStorage.removeItem(passwordKey);
  showSession(false);
  byID("password").value = "";
  byID("manual-form").reset();
  filtersForm.reset();
  clearCheckerPage();
  clearRunners();
  for (const id of ["flags-body", "teams-body", "summary-body"]) {
    byID(id).replaceChildren();
  }
  message("manual-message");
  message("login-message", reason, "error");
  byID("password").focus();
}

async function api(path, { payload, signal, credential = password, method = payload === undefined ? "GET" : "POST", cache } = {}) {
  const headers = { Accept: "application/json", Authorization: credential };
  const options = { headers, signal, method, cache };
  if (payload !== undefined) {
    headers["Content-Type"] = "application/json";
    options.body = JSON.stringify(payload);
  }
  const response = await fetch(`/api/${path}`, options);
  if ([401, 403].includes(response.status)) {
    if (path === "login") throw new Error("Incorrect server password.");
    if (credential === password) signOut("Sign in again to continue.");
    throw new Error("Access denied.");
  }

  const contentType = response.headers.get("content-type") || "";
  const body = await response.text();
  const data = body && contentType.includes("application/json") ? JSON.parse(body) : undefined;
  if (!response.ok) {
    const detail = data?.error || data?.message || (contentType.includes("text/plain") ? body.trim() : "");
    throw new Error(detail || `Request failed (${response.status}).`);
  }
  return data;
}

async function signIn(candidate) {
  const button = byID("login-form").querySelector("button");
  button.disabled = true;
  message("login-message");
  try {
    await api("login", { payload: { password: candidate }, credential: candidate });
    password = candidate;
    sessionStorage.setItem(passwordKey, password);
    showSession(true);
    byID("password").value = "";
    await refresh(1);
  } catch (error) {
    signOut(error.message);
  } finally {
    button.disabled = false;
  }
}

function addCell(row, value, className = "", copyable = false) {
  const cell = row.insertCell();
  cell.className = className;
  cell.textContent = value === null || value === undefined || value === "" ? "—" : value;
  if (copyable && value) {
    const text = cell.firstChild;
    const button = document.createElement("button");
    button.type = "button";
    button.className = "copy";
    button.textContent = "Copy";
    button.addEventListener("click", async () => {
      if (navigator.clipboard) {
        try {
          await navigator.clipboard.writeText(value);
          button.textContent = "Copied";
          return;
        } catch {
          // Clipboard permission varies with the browser and connection.
        }
      }
      const selection = window.getSelection();
      const range = document.createRange();
      range.selectNodeContents(text);
      selection.removeAllRanges();
      selection.addRange(range);
      message("page-message", "Text selected. Use your browser's copy command.");
    });
    cell.append(" ", button);
  }
  return cell;
}

function showRows(id, rows, empty, columns) {
  const body = byID(id);
  body.replaceChildren();
  if (rows.length) {
    for (const row of rows) body.appendChild(row);
  } else {
    const cell = addCell(body.insertRow(), empty, "empty");
    cell.colSpan = columns;
  }
}

function setOptions(id, values) {
  const options = (values || []).map((value) => {
    const option = document.createElement("option");
    option.value = value;
    return option;
  });
  byID(id).replaceChildren(...options);
}

function dateFormatter(timezone) {
  const options = { dateStyle: "short", timeStyle: "medium" };
  try {
    return new Intl.DateTimeFormat(undefined, { ...options, timeZone: timezone || undefined });
  } catch {
    return new Intl.DateTimeFormat(undefined, options);
  }
}

function formatTime(formatter, timestamp) {
  if (timestamp === null || timestamp === undefined) return "";
  const date = new Date(Number(timestamp) * 1000);
  return Number.isNaN(date.valueOf()) ? "" : formatter.format(date);
}

function updatePager(loading = false) {
  byID("page-number").textContent = `${page} / ${lastPage}`;
  for (const button of pageButtons) {
    const before = ["first", "previous"].includes(button.dataset.page);
    button.disabled = loading || (before ? page <= 1 : page >= lastPage);
  }
}

async function loadFlags(targetPage, signal) {
  const query = new URLSearchParams(filters);
  query.set("page", targetPage);
  query.set("page_size", pageSize);
  const [data, config] = await Promise.all([
    api(`filter_flags?${query}`, { signal }),
    api("filter_config", { signal }),
  ]);
  if (signal.aborted) return;
  lastPage = Math.max(1, Math.ceil(data.total / (data.page_size || pageSize)));
  if (targetPage > lastPage) {
    await loadFlags(lastPage, signal);
    return;
  }
  page = data.page || targetPage;
  const { sploit, team, status } = config.filters || {};
  setOptions("exploit-options", sploit);
  setOptions("team-options", team);
  setOptions("status-options", status);
  byID("server-timezone").textContent = config.server_tz ? `Filter times use ${config.server_tz}.` : "";
  byID("flag-format").textContent = config.flag_format ? `Format: ${config.flag_format}` : "";
  const formatter = dateFormatter(config.server_tz);

  const rows = (data.flags || []).map((flag) => {
    const row = document.createElement("tr");
    addCell(row, flag.sploit, "", true);
    addCell(row, flag.team);
    addCell(row, flag.flag, "mono", true);
    addCell(row, formatTime(formatter, flag.time), "nowrap");
    const statusClass = { ACCEPTED: "accepted", QUEUED: "queued", REJECTED: "rejected" };
    addCell(row, flag.status, `status ${statusClass[flag.status] || ""}`);
    addCell(row, flag.checksystem_response, "", true);
    return row;
  });
  showRows("flags-body", rows, "No flags match these filters.", 6);
  byID("flag-count").textContent = `${data.total} ${data.total === 1 ? "flag" : "flags"}`;
}

async function loadTeams(signal) {
  const teams = (await api("teams", { signal })) || [];
  if (signal.aborted) return;
  const rows = teams.map((team) => {
    const row = document.createElement("tr");
    addCell(row, team.name);
    addCell(row, team.address, "mono", true);
    return row;
  });
  showRows("teams-body", rows, "No targets configured.", 2);
  byID("team-count").textContent = `${teams.length} ${teams.length === 1 ? "target" : "targets"}`;
}

async function loadSummary(signal) {
  const path = byID("summary-response").checked ? "summary?response=1" : "summary";
  const summary = (await api(path, { signal })) || [];
  if (signal.aborted) return;
  const rows = summary.flatMap((exploit) => (exploit.teams || []).map((team) => {
    const row = document.createElement("tr");
    addCell(row, exploit.sploit_name);
    addCell(row, team.team);
    addCell(row, team.team_status === "not status" ? "" : team.team_status, "counts mono");
    return row;
  }));
  showRows("summary-body", rows, "No flags to summarize.", 3);
}

function clearRunnerToken() {
  const credential = byID("runner-credential");
  credential.hidden = true;
  delete credential.dataset.runnerId;
  byID("runner-token").value = "";
  message("runner-copy-message");
}

function setRunnerBusy(busy) {
  byID("runner-create").disabled = busy;
  for (const button of document.querySelectorAll("[data-revoke-runner]")) button.disabled = busy;
}

function clearRunners() {
  clearTimeout(runnerPoll);
  runnerRequest?.abort();
  runnerRequest = undefined;
  clearRunnerToken();
  runnerForm.reset();
  setRunnerBusy(false);
  byID("runners-body").replaceChildren();
  byID("runners-count").textContent = "";
  message("runners-message");
}

function runnerLastSeen(value) {
  if (!value || value.startsWith("0001-01-01")) return "Never";
  const time = new Date(value);
  return Number.isNaN(time.valueOf()) ? "—" : time.toISOString().replace(/\.000Z$/, "Z");
}

async function loadRunners(signal) {
  const runners = (await api("runners", { signal, cache: "no-store" })) || [];
  if (signal.aborted) return;
  const rows = runners.map((runner) => {
    const row = document.createElement("tr");
    const name = addCell(row, runner.name);
    const details = document.createElement("div");
    details.className = "muted mono runner-details";
    details.textContent = runner.version ? `${runner.id} · ${runner.version}` : runner.id;
    name.append(details);
    addCell(row, runner.address, "mono runner-address");

    const connection = runner.revoked ? "Revoked" : runner.online ? "Online" : "Offline";
    const tone = runner.revoked ? "rejected" : runner.online ? "accepted" : "";
    addCell(row, connection, `status ${tone}`);
    addCell(row, runnerLastSeen(runner.last_seen), "mono nowrap");
    const counts = runnerJobStatuses.map((status) => `${status} ${runner.jobs?.[status] ?? 0}`).join(" · ");
    addCell(row, counts, "runner-jobs");

    const actions = row.insertCell();
    if (!runner.revoked) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "secondary";
      button.dataset.revokeRunner = runner.id;
      button.textContent = "Revoke";
      button.disabled = Boolean(runnerRequest);
      button.addEventListener("click", () => changeRunner(runner.id));
      actions.append(button);
    }
    return row;
  });
  showRows("runners-body", rows, "No runners registered.", 6);
  byID("runners-count").textContent = `${runners.length} ${runners.length === 1 ? "runner" : "runners"}`;
}

async function changeRunner(id, name) {
  if (!password || runnerRequest || window.location.hash !== "#runners") return;
  const controller = new AbortController();
  const signal = controller.signal;
  const registering = id === undefined;
  runnerRequest = controller;
  setRunnerBusy(true);
  if (registering) clearRunnerToken();
  message("runners-message", registering ? "Registering…" : "Revoking…");
  try {
    if (registering) {
      const { runner, token } = await api("runners", { payload: { name }, signal, cache: "no-store" });
      if (signal.aborted || window.location.hash !== "#runners") return;
      byID("runner-token").value = token;
      byID("runner-credential").dataset.runnerId = runner.id;
      byID("runner-credential").hidden = false;
      runnerForm.reset();
      message("runners-message", "Runner registered.", "success");
    } else {
      await api(`runners/${encodeURIComponent(id)}`, { method: "DELETE", signal, cache: "no-store" });
      if (signal.aborted || window.location.hash !== "#runners") return;
      if (byID("runner-credential").dataset.runnerId === String(id)) clearRunnerToken();
      message("runners-message", "Runner revoked.", "success");
    }
    await refresh(page, true);
  } catch (error) {
    if (!signal.aborted) message("runners-message", error.message, "error");
  } finally {
    if (runnerRequest === controller) {
      runnerRequest = undefined;
      setRunnerBusy(false);
    }
  }
}

function checkerInput(name) {
  return checkerForm.elements.namedItem(name);
}

function checkerJSONMode() {
  return !byID("checker-json-fields").hidden;
}

function updateCheckerFields() {
  const preset = checkerPresets[checkerProtocol.value];
  const usesURL = preset.fields.includes("url");
  const usesHost = preset.fields.includes("host");
  for (const label of checkerFields) {
    const name = label.dataset.checkerField;
    const input = checkerInput(name);
    label.hidden = !preset.fields.includes(name);
    input.disabled = label.hidden || checkerJSONMode();
    input.required = !input.disabled;
  }
  if (usesURL && usesHost) {
    checkerInput("url").required = false;
    checkerInput("host").required = !checkerInput("url").value.trim();
    checkerInput("port").required = false;
  }
  const protocol = preset.protocol ?? checkerProtocol.value;
  const optionsReset = protocol !== checkerOptionsProtocol && Object.keys(checkerOptions).length > 0;
  byID("checker-hint").textContent = optionsReset ? preset.hint + " Saving a different protocol clears the previous advanced options." : preset.hint;
}

function setCheckerMode(json) {
  byID("checker-preset-fields").hidden = json;
  checkerProtocol.disabled = json;
  byID("checker-json-fields").hidden = !json;
  checkerJSON.disabled = !json;
  byID("checker-mode").textContent = json ? "Use form" : "Edit JSON";
  if (!json) checkerJSON.value = "";
  updateCheckerFields();
}

function clearCheckerForm() {
  checkerSaveRequest?.abort();
  checkerSaveRequest = undefined;
  checkerOptions = {};
  checkerOptionsProtocol = "";
  checkerForm.reset();
  setCheckerMode(false);
  checkerControls.disabled = true;
  message("checker-message");
}

function clearCheckerPage() {
  clearCheckerForm();
  submissionSaveRequest?.abort();
  submissionSaveRequest = undefined;
  submissionForm.reset();
  submissionControls.disabled = true;
  message("submission-message");
}

function fillCheckerForm(config) {
  if (checkerJSONMode()) {
    checkerJSON.value = JSON.stringify(config, null, 2);
    return;
  }
  const omctf = checkerPresets.omctf;
  checkerProtocol.value = config.protocol === omctf.protocol && config.url === omctf.defaultURL ? "omctf" : (config.protocol ?? "");
  checkerOptions = config.options ?? {};
  checkerOptionsProtocol = config.protocol ?? "";
  for (const label of checkerFields) {
    const name = label.dataset.checkerField;
    checkerInput(name).value = config[name] || "";
  }
  updateCheckerFields();
}

function readCheckerForm() {
  const preset = checkerPresets[checkerProtocol.value];
  const protocol = preset.protocol ?? checkerProtocol.value;
  const config = { protocol, url: "", host: "", port: 0, token: "", team_id: "", options: {} };
  for (const name of preset.fields) {
    const value = checkerInput(name).value;
    config[name] = name === "port" ? Number(value) : name === "token" ? value : value.trim();
  }
  if (protocol && protocol === checkerOptionsProtocol) config.options = checkerOptions;
  return config;
}

function readCheckerJSON() {
  let config;
  try {
    config = JSON.parse(checkerJSON.value);
  } catch {
    throw new Error("Enter valid JSON.");
  }
  if (!config || typeof config !== "object" || Array.isArray(config)) {
    throw new Error("Checker configuration must be a JSON object.");
  }
  return config;
}

function fillSubmissionSettings(config) {
  for (const input of submissionFields) input.value = config[input.name];
}

function readSubmissionSettings() {
  const config = {};
  for (const input of submissionFields) {
    config[input.name] = input.type === "number" ? input.valueAsNumber : input.value;
  }
  return config;
}

async function loadSettings(path, controls, messageID, fill, signal) {
  controls.disabled = true;
  message(messageID);
  try {
    const config = await api(path, { signal, cache: "no-store" });
    if (signal.aborted) return;
    fill(config);
    controls.disabled = false;
  } catch (error) {
    if (!signal.aborted) message(messageID, error.message, "error");
  }
}

async function refresh(targetPage = page, silent = false) {
  if (!password) return;
  const requestedView = window.location.hash.slice(1);
  const view = Object.hasOwn(views, requestedView) ? requestedView : "flags";
  if (view === "checker" && (checkerSaveRequest || submissionSaveRequest)) return;
  if (view !== "checker" && !byID("checker-view").hidden) clearCheckerPage();
  if (view !== "runners" && !byID("runners-view").hidden) clearRunners();
  clearTimeout(runnerPoll);
  pendingRequest?.abort();
  const controller = new AbortController();
  const signal = controller.signal;
  pendingRequest = controller;
  for (const name of Object.keys(views)) {
    const link = document.querySelector(`#navigation [href="#${name}"]`);
    byID(`${name}-view`).hidden = name !== view;
    if (name === view) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  }
  byID("page-title").textContent = views[view];
  updateRefreshButton();
  updatePager(true);
  if (!silent) message("page-message", "Loading…");
  try {
    if (view === "flags") await loadFlags(targetPage, signal);
    else if (view === "teams") await loadTeams(signal);
    else if (view === "summary") await loadSummary(signal);
    else if (view === "runners") await loadRunners(signal);
    else await Promise.all([
      loadSettings("checker_config", checkerControls, "checker-message", fillCheckerForm, signal),
      loadSettings("submission_settings", submissionControls, "submission-message", fillSubmissionSettings, signal),
    ]);
    if (!signal.aborted) message("page-message");
  } catch (error) {
    if (!signal.aborted) message("page-message", error.message, "error");
  } finally {
    if (pendingRequest === controller) {
      pendingRequest = undefined;
      updateRefreshButton();
      updatePager();
      if (view === "runners" && password && window.location.hash === "#runners" && !document.hidden) {
        runnerPoll = setTimeout(() => refresh(page, true), 5000);
      }
    }
  }
}

byID("login-form").addEventListener("submit", (event) => {
  event.preventDefault();
  void signIn(byID("password").value);
});
byID("logout").addEventListener("click", () => signOut());
byID("refresh").addEventListener("click", () => refresh());
byID("summary-response").addEventListener("change", () => refresh());
window.addEventListener("hashchange", () => refresh());
window.addEventListener("pagehide", clearRunnerToken);
document.addEventListener("visibilitychange", () => {
  if (document.hidden) clearTimeout(runnerPoll);
  else if (window.location.hash === "#runners") void refresh(page, true);
});

runnerForm.addEventListener("submit", (event) => {
  event.preventDefault();
  const name = runnerForm.elements.namedItem("name").value.trim();
  if (!name) {
    message("runners-message", "Enter a runner name.", "error");
    return;
  }
  void changeRunner(undefined, name);
});
byID("dismiss-runner-token").addEventListener("click", clearRunnerToken);
byID("copy-runner-token").addEventListener("click", async () => {
  const input = byID("runner-token");
  const token = input.value;
  if (!token) return;
  let copied = false;
  try {
    if (navigator.clipboard) {
      await navigator.clipboard.writeText(token);
      copied = true;
    }
  } catch {
    // Plain HTTP and browser permissions can require the selection fallback.
  }
  if (input.value !== token) return;
  if (!copied) {
    input.select();
    try {
      copied = document.execCommand("copy");
    } catch {
      // The selected token remains available for a manual copy.
    }
  }
  message("runner-copy-message", copied ? "Token copied." : "Token selected. Use your browser's copy command.", copied ? "success" : "");
});

checkerProtocol.addEventListener("change", () => {
  const { defaultURL } = checkerPresets[checkerProtocol.value];
  if (defaultURL) {
    checkerInput("url").value = defaultURL;
    checkerInput("token").value = "";
  }
  updateCheckerFields();
});
checkerInput("url").addEventListener("input", updateCheckerFields);
byID("checker-mode").addEventListener("click", () => {
  if (!checkerJSONMode()) {
    checkerJSON.value = JSON.stringify(readCheckerForm(), null, 2);
    checkerInput("token").value = "";
    setCheckerMode(true);
    checkerJSON.focus();
    message("checker-message");
    return;
  }
  try {
    const config = readCheckerJSON();
    const protocol = config.protocol ?? "";
    if (!Object.hasOwn(checkerPresets, protocol) || checkerPresets[protocol].protocol) {
      throw new Error("Choose an existing protocol ID before using the form.");
    }
    setCheckerMode(false);
    fillCheckerForm(config);
    checkerProtocol.focus();
    message("checker-message");
  } catch (error) {
    message("checker-message", error.message, "error");
  }
});

checkerForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (checkerControls.disabled) return;
  let config;
  try {
    config = checkerJSONMode() ? readCheckerJSON() : readCheckerForm();
    if (!checkerJSONMode() && config.url) {
      const url = new URL(config.url);
      if (!["http:", "https:"].includes(url.protocol) || !url.hostname || url.username || url.password || url.hash) {
        throw new Error("Enter an HTTP(S) URL without credentials or a fragment.");
      }
    }
  } catch (error) {
    message("checker-message", error.message, "error");
    return;
  }

  const controller = new AbortController();
  checkerSaveRequest = controller;
  checkerControls.disabled = true;
  updateRefreshButton();
  message("checker-message", "Saving…");
  try {
    const saved = await api("checker_config", { method: "PUT", payload: config, signal: controller.signal, cache: "no-store" });
    if (controller.signal.aborted) return;
    fillCheckerForm(saved);
    message("checker-message", saved.protocol ? "Checker saved. Submission is enabled." : "Checker saved. Submission is disabled.", "success");
  } catch (error) {
    if (!controller.signal.aborted) message("checker-message", error.message, "error");
  } finally {
    if (checkerSaveRequest === controller) {
      checkerSaveRequest = undefined;
      checkerControls.disabled = false;
      updateRefreshButton();
    }
  }
});

byID("submission-omctf").addEventListener("click", () => {
  submissionForm.elements.namedItem("flag_format").value = "[A-Z0-9]{31}=";
  submissionForm.elements.namedItem("flag_lifetime_seconds").value = "600";
  submissionForm.elements.namedItem("submit_period_seconds").value = "2";
  message("submission-message", "OmCTF values filled. Review the batch limit and save to apply.");
});

submissionForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (submissionControls.disabled) return;
  const config = readSubmissionSettings();
  const controller = new AbortController();
  submissionSaveRequest = controller;
  submissionControls.disabled = true;
  updateRefreshButton();
  message("submission-message", "Saving…");
  try {
    const saved = await api("submission_settings", { method: "PUT", payload: config, signal: controller.signal, cache: "no-store" });
    if (controller.signal.aborted) return;
    fillSubmissionSettings(saved);
    message("submission-message", "Flag settings saved.", "success");
  } catch (error) {
    if (!controller.signal.aborted) message("submission-message", error.message, "error");
  } finally {
    if (submissionSaveRequest === controller) {
      submissionSaveRequest = undefined;
      submissionControls.disabled = false;
      updateRefreshButton();
    }
  }
});

filtersForm.addEventListener("submit", (event) => {
  event.preventDefault();
  filters = new URLSearchParams();
  for (const [key, value] of new FormData(filtersForm)) {
    if (value) filters.set(key, (key === "since" || key === "until") ? value.replace("T", " ") : value);
  }
  void refresh(1);
});
filtersForm.addEventListener("reset", () => {
  filters = new URLSearchParams();
  void refresh(1);
});
for (const button of pageButtons) {
  button.addEventListener("click", () => {
    const pages = { first: 1, previous: page - 1, next: page + 1, last: lastPage };
    void refresh(pages[button.dataset.page]);
  });
}

byID("manual-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const form = event.currentTarget;
  const input = byID("manual-text");
  const text = input.value;
  const button = form.querySelector("button");
  const credential = password;
  button.disabled = true;
  message("manual-message", "Sending…");
  try {
    const { queued } = await api("post_flags", { payload: { text }, credential });
    if (credential !== password) return;
    if (input.value === text) form.reset();
    message("manual-message", `Sent ${queued} ${queued === 1 ? "flag" : "flags"} to the farm.`, "success");
    await refresh(1);
  } catch (error) {
    if (credential === password) message("manual-message", error.message, "error");
  } finally {
    button.disabled = false;
  }
});

const savedPassword = sessionStorage.getItem(passwordKey);
if (savedPassword) void signIn(savedPassword);
