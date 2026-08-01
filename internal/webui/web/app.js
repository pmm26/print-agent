"use strict";

const $ = (sel) => document.querySelector(sel);
const api = async (path, opts = {}) => {
  const res = await fetch("/api/v1" + path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  const body = res.status === 204 ? null : await res.json().catch(() => null);
  if (!res.ok) {
    const error = new Error(body?.error || res.statusText);
    error.status = res.status;
    error.code = body?.code || "";
    throw error;
  }
  return body;
};

let toastTimer;
function toast(msg, isError = false) {
  const el = $("#toast");
  el.textContent = msg;
  el.style.background = isError ? "var(--bad)" : "";
  el.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.add("hidden"), 3500);
}
const call = (fn, okMsg) => fn().then(() => { if (okMsg) toast(okMsg); refreshStatus(); refreshQueue(); })
  .catch((e) => toast(e.message, true));

// ---- dashboard navigation ----
const defaultDashboardPath = "/admin/operations/jobs";
const dashboardRoutes = {
  "/admin/setup/pair": { panel: "bluetooth", category: "setup", title: "Pair devices" },
  "/admin/setup/printers": { panel: "printers", category: "setup", title: "Printers" },
  "/admin/setup/pos": { panel: "pairing", category: "setup", title: "POS Pairing" },
  "/admin/operations/jobs": { panel: "jobs", category: "operations", title: "Jobs" },
  "/admin/operations/queue": { panel: "queue", category: "operations", title: "Queue" },
  "/admin/system/diagnostics": { panel: "diagnostics", category: "system", title: "Diagnostics" },
  "/admin/system/logs": { panel: "logs", category: "system", title: "Logs" },
  "/admin/dev/pos-simulator": { panel: "dev", category: "dev", title: "POS Simulator" },
};
const routeForPanel = Object.fromEntries(Object.entries(dashboardRoutes).map(([path, page]) => [page.panel, path]));

function closeNavigationMenus() {
  document.querySelectorAll(".nav-menu[open]").forEach((menu) => menu.removeAttribute("open"));
}

function activateDashboardPath(path) {
  const page = dashboardRoutes[path] || dashboardRoutes[defaultDashboardPath];
  document.querySelectorAll(".panel").forEach((panel) => panel.classList.toggle("hidden", panel.id !== `tab-${page.panel}`));
  document.querySelectorAll(".nav-menu").forEach((menu) =>
    menu.classList.toggle("active", menu.dataset.category === page.category));
  document.querySelectorAll("[data-route]").forEach((link) => {
    if (link.getAttribute("href") === path) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  });
  document.title = `${page.title} · Print Agent`;

  const refreshers = {
    jobs: refreshJobs,
    bluetooth: refreshBluetoothDevices,
    printers: refreshStatus,
    queue: refreshQueue,
    pairing: refreshPairing,
    diagnostics: refreshDiagnostics,
    logs: refreshLogs,
    dev: refreshDevSimulator,
  };
  refreshers[page.panel]?.();
}

function navigateDashboard(path, { replace = false } = {}) {
  if (!dashboardRoutes[path]) path = defaultDashboardPath;
  if (window.location.pathname !== path) {
    window.history[replace ? "replaceState" : "pushState"]({}, "", path);
  }
  closeNavigationMenus();
  activateDashboardPath(path);
}

function activateTab(name) {
  navigateDashboard(routeForPanel[name] || defaultDashboardPath);
}

function initializeNavigation() {
  document.querySelectorAll("[data-route]").forEach((link) => {
    link.addEventListener("click", (event) => {
      if (event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
      event.preventDefault();
      navigateDashboard(link.getAttribute("href"));
    });
  });
  document.querySelectorAll(".nav-menu").forEach((menu) => {
    menu.addEventListener("toggle", () => {
      if (!menu.open) return;
      document.querySelectorAll(".nav-menu[open]").forEach((other) => {
        if (other !== menu) other.removeAttribute("open");
      });
    });
  });
  document.addEventListener("click", (event) => {
    if (!event.target.closest("#dashboard-nav")) closeNavigationMenus();
  });
  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape") closeNavigationMenus();
  });
  window.addEventListener("popstate", () => activateDashboardPath(window.location.pathname));

  const initialPath = dashboardRoutes[window.location.pathname] ? window.location.pathname : defaultDashboardPath;
  if (initialPath !== window.location.pathname) window.history.replaceState({}, "", initialPath);
  activateDashboardPath(initialPath);
}

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const fmtTime = (iso) => (iso ? new Date(iso).toLocaleTimeString() : "—");

// ---- Jobs ----
async function refreshJobs() {
  if ($("#tab-jobs").classList.contains("hidden")) return;
  const jobId = $("#jobs-filter").value.trim();
  const result = await api(`/jobs?limit=100${jobId ? "&jobId=" + encodeURIComponent(jobId) : ""}`).catch(() => ({ jobs: [] }));
  const rows = result.jobs || [];
  $("#jobs-table tbody").innerHTML = rows.map((job) => {
    const flags = [job.requiresAttention ? "⚠ attention" : "", job.hasUncertainResult ? "? uncertain" : "",
      job.hasManualReprints ? "reprints" : ""].filter(Boolean).join(" · ");
    return `<tr data-job-uid="${esc(job.uid)}" class="clickable">
      <td><b>${esc(job.jobId)}</b><div class="muted">${esc(job.uid)}</div></td>
      <td>${esc(job.template)}</td><td>${fmtTime(job.createdAt)}</td>
      <td><span class="badge ${esc(job.state)}">${esc(job.state)}</span></td>
      <td>${job.fulfilledPrinterCount}/${job.originalPrinterCount}</td><td>${esc(flags)}</td></tr>`;
  }).join("") || `<tr><td colspan="6" class="muted">No Jobs</td></tr>`;
  $("#jobs-table").querySelectorAll("tr[data-job-uid]").forEach((row) =>
    row.addEventListener("click", () => openJob(row.dataset.jobUid)));
}

async function openJob(uid) {
  try {
    const job = await api(`/jobs/${encodeURIComponent(uid)}`);
    $("#job-dialog-title").textContent = `${job.jobId} / ${job.template}`;
    $("#job-detail").innerHTML = `<div class="card">
      <div><b>UID:</b> ${esc(job.uid)}</div><div><b>State:</b> ${esc(job.state)}</div>
      <div><b>Original printers:</b> ${job.fulfilledPrinterCount}/${job.originalPrinterCount} fulfilled</div>
      <pre>${esc(JSON.stringify(job.data, null, 2))}</pre></div>` +
      (job.originalPrinters || []).map((printer) => `<div class="card job-printer" data-printer-id="${esc(printer.printerId)}">
        <h3><label><input type="checkbox" data-reprint-printer value="${esc(printer.printerId)}"> ${esc(printer.printerId)}</label>
          <span class="badge ${printer.fulfilled ? "connected" : printer.cancelled ? "cancelled" : "error"}">
          ${printer.fulfilled ? "fulfilled" : printer.cancelled ? "cancelled" : "unfulfilled"}</span></h3>
        <table><thead><tr><th>#</th><th>UID</th><th>Trigger</th><th>Status</th><th>Error</th></tr></thead><tbody>
        ${(printer.runs || []).map((run) => `<tr><td>${run.runNumber}</td><td>${esc(run.uid)}</td>
          <td>${esc(run.trigger)}</td><td>${esc(run.status)}${run.retryPending ? " · retry pending" : ""}</td>
          <td>${esc(run.errorMessage || "")}</td></tr>`).join("")}</tbody></table></div>`).join("") +
      `<div class="toolbar"><button id="btn-job-reprint" class="primary">Reprint selected</button>
        <button id="btn-job-cancel" class="danger">Cancel selected targets</button></div>`;
    $("#btn-job-reprint").addEventListener("click", async () => {
      const printerIds = [...$("#job-detail").querySelectorAll("[data-reprint-printer]:checked")].map((el) => el.value);
      if (!printerIds.length) return toast("Select at least one printer", true);
      const uncertain = (job.originalPrinters || []).some((p) => printerIds.includes(p.printerId) && (p.runs || []).some((r) => r.status === "uncertain" && !r.resolution));
      const warning = uncertain ? "The earlier result is uncertain and may already have printed. " : "";
      if (!confirm(`${warning}Create ${printerIds.length} physical reprint(s)?`)) return;
      const reason = prompt("Reason for reprint (optional):", "") || "";
      await api(`/jobs/${encodeURIComponent(job.uid)}/reprint`, { method: "POST", body: JSON.stringify({
        reprintRequestId: crypto.randomUUID(), printerIds, reason,
      }) });
      toast("Reprint queued");
      await openJob(job.uid);
      refreshJobs();
      refreshQueue();
    });
    $("#btn-job-cancel").addEventListener("click", async () => {
      const printerIds = [...$("#job-detail").querySelectorAll("[data-reprint-printer]:checked")].map((el) => el.value);
      if (!printerIds.length) return toast("Select at least one printer", true);
      if (!confirm(`Cancel pending output for ${printerIds.length} original printer target(s)?`)) return;
      const reason = prompt("Cancellation reason (optional):", "") || "";
      await api(`/jobs/${encodeURIComponent(job.uid)}/cancel`, { method: "POST", body: JSON.stringify({ printerIds, reason }) });
      toast("Targets cancelled");
      await openJob(job.uid);
      refreshJobs();
      refreshQueue();
    });
    if (!$("#job-dialog").open) $("#job-dialog").showModal();
  } catch (e) { toast(e.message, true); }
}

$("#btn-jobs-refresh").addEventListener("click", refreshJobs);
$("#jobs-filter").addEventListener("change", refreshJobs);
$("#btn-job-close").addEventListener("click", () => $("#job-dialog").close());

// ---- Bluetooth discovery & device pairing ----
let scanning = false;
let scanTimer;
const pairingAddresses = new Set();

function setScanning(active) {
  scanning = active;
  $("#btn-scan").disabled = active;
  $("#btn-stop-scan").disabled = !active;
  $("#scan-status").textContent = active
    ? "Scanning… Keep each printer powered on and in pairing mode."
    : "Scan stopped. Paired devices remain available below.";
}

async function refreshBluetoothDevices() {
  if ($("#tab-bluetooth").classList.contains("hidden")) return false;
  const root = $("#device-list");
  try {
    const result = await api("/bluetooth/devices");
    if (!result.supported) {
      $("#scan-status").textContent = "In-app pairing is available on Linux. Use your operating system Bluetooth settings here.";
      $("#btn-scan").disabled = true;
      $("#btn-stop-scan").disabled = true;
      root.innerHTML = `<div class="card"><button id="btn-pair-open-bt">Open Bluetooth settings</button></div>`;
      $("#btn-pair-open-bt").addEventListener("click", () =>
        call(() => api("/system/open-bluetooth-settings", { method: "POST" })));
      return false;
    }
    renderBluetoothDevices(result.devices || []);
    return true;
  } catch (e) {
    root.innerHTML = `<div class="card error">${esc(e.message)}</div>`;
    return false;
  }
}

function renderBluetoothDevices(devices) {
  const root = $("#device-list");
  if (!devices.length) {
    root.innerHTML = `<div class="card muted">${scanning ? "Looking for nearby Bluetooth devices…" : "No devices found yet. Select Scan for devices."}</div>`;
    return;
  }
  root.innerHTML = devices.map((device) => {
    const busy = pairingAddresses.has(device.address);
    const state = device.paired
      ? `<span class="badge connected">paired</span>`
      : device.endpoint
        ? `<span class="badge connected">ready without pairing</span>`
        : `<span class="badge disabled">not paired</span>`;
    const connected = device.connected ? `<span class="badge printing">connected</span>` : "";
    const printer = device.isPrinter ? `<span class="device-kind">🖨 Likely printer</span>` : "";
    const action = device.endpoint
      ? `<button class="primary" data-device-act="add" data-endpoint="${esc(device.endpoint)}">Add printer</button>`
      : device.paired
        ? `<span class="muted">Paired, but no printer endpoint was found.</span>`
        : `<input class="pin-input" data-device-pin placeholder="PIN (default 0000)" inputmode="numeric" maxlength="16">
           <button class="primary" data-device-act="pair" ${busy ? "disabled" : ""}>${busy ? "Pairing…" : "Pair device"}</button>`;
    return `<div class="card device" data-address="${esc(device.address)}">
      <div class="device-info">
        <h2>${esc(device.name || "Unknown device")} ${state} ${connected}</h2>
        <div class="meta">${esc(device.address)} ${printer}</div>
      </div>
      <div class="device-actions">${action}</div>
    </div>`;
  }).join("");

  root.querySelectorAll("button[data-device-act=pair]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const card = btn.closest(".device");
      const address = card.dataset.address;
      const pin = card.querySelector("[data-device-pin]").value.trim();
      pairingAddresses.add(address);
      renderBluetoothDevices(devices);
      await stopBluetoothDiscovery(true);
      try {
        const result = await api(`/bluetooth/devices/${encodeURIComponent(address)}/pair`, {
          method: "POST",
          body: JSON.stringify({ pin }),
        });
        toast(result.ready
          ? `${address} is ready. You can add it as a printer.`
          : `Paired ${address}, but no supported printer endpoint was found.`);
      } catch (e) {
        toast(e.message, true);
      } finally {
        pairingAddresses.delete(address);
        refreshBluetoothDevices();
      }
    });
  });
  root.querySelectorAll("button[data-device-act=add]").forEach((btn) => {
    btn.addEventListener("click", () => {
      activateTab("printers");
      openDialog(null, btn.dataset.endpoint);
    });
  });
}

async function startBluetoothDiscovery() {
  clearTimeout(scanTimer);
  try {
    await api("/bluetooth/discovery/start", { method: "POST" });
    setScanning(true);
    await refreshBluetoothDevices();
    scanTimer = setTimeout(() => stopBluetoothDiscovery(), 20000);
  } catch (e) {
    setScanning(false);
    toast(e.message, true);
  }
}

async function stopBluetoothDiscovery(silent = false) {
  clearTimeout(scanTimer);
  if (!scanning) return;
  try {
    await api("/bluetooth/discovery/stop", { method: "POST" });
  } catch (e) {
    if (!silent) toast(e.message, true);
  } finally {
    setScanning(false);
    refreshBluetoothDevices();
  }
}

$("#btn-scan").addEventListener("click", startBluetoothDiscovery);
$("#btn-stop-scan").addEventListener("click", () => stopBluetoothDiscovery());

// ---- printers ----
async function refreshStatus() {
  try {
    const data = await api("/status");
    $("#agent-state").textContent = "Running";
    $("#agent-state").className = "badge running";
    const persistence = data.persistence || {};
    const alert = $("#persistence-alert");
    alert.textContent = persistence.paused
      ? `Printing is paused while queue state is being saved: ${persistence.reason || "database unavailable"}`
      : data.degraded ? "Queue status is temporarily unavailable; check diagnostics." : "";
    alert.classList.toggle("hidden", !persistence.paused && !data.degraded);
    renderPrinters(data.printers || []);
  } catch {
    $("#agent-state").textContent = "Unreachable";
    $("#agent-state").className = "badge error";
  }
}

let knownPrinters = [];
function renderPrinters(list) {
	knownPrinters = list;
	renderDevPrinters(list);
	const selected = $("#queue-printer")?.value || "";
	if ($("#queue-printer")) {
		$("#queue-printer").innerHTML = `<option value="">All printers</option>` + list.map((st) =>
			`<option value="${esc(st.printer.id)}">${esc(st.printer.displayName)}</option>`).join("");
		$("#queue-printer").value = selected;
	}
  const root = $("#printer-list");
  if (!list.length) {
    root.innerHTML = `<div class="card muted">No printers configured yet. Pair devices in the first tab, then use <b>Add printer</b>.</div>`;
    return;
  }
  root.innerHTML = list.map((st) => {
    const p = st.printer;
    const attn = st.attentionCount ? `<span class="attn">⚠ ${st.attentionCount} need attention</span>` : "";
    const retry = st.nextRetryAt ? ` · retry ${fmtTime(st.nextRetryAt)} (attempt ${st.reconnectAttempt})` : "";
    return `<div class="card printer" data-id="${esc(p.id)}">
      <div>
        <h2>${esc(p.displayName)} <span class="badge ${esc(st.state)}">${esc(st.state)}</span></h2>
        <div class="meta"><b>${esc(st.endpoint || "no endpoint")}</b> · queue: <b>${st.queueDepth}</b> ·
          last transmission: ${fmtTime(st.lastTransmission)}${retry} ${attn}</div>
        ${st.lastError ? `<div class="error">${esc(st.lastError)}</div>` : ""}
      </div>
      <div class="actions">
        <button data-act="test">Test print</button>
        <button data-act="reconnect">Reconnect</button>
        <button data-act="configure">Configure</button>
        <button data-act="toggle">${p.enabled ? "Disable" : "Enable"}</button>
        <button data-act="remove" class="danger">Remove</button>
      </div>
    </div>`;
  }).join("");

  root.querySelectorAll("button[data-act]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const id = btn.closest(".printer").dataset.id;
      const p = list.find((s) => s.printer.id === id)?.printer;
      switch (btn.dataset.act) {
        case "test": return call(() => api(`/printers/${id}/test`, { method: "POST" }), `Test print queued for ${id}`);
        case "reconnect": return call(() => api(`/printers/${id}/reconnect`, { method: "POST" }), `Reconnecting ${id}`);
        case "toggle": return call(() => api(`/printers/${id}/${p.enabled ? "disable" : "enable"}`, { method: "POST" }));
        case "remove":
          if (confirm(`Remove printer "${id}"? Its history is kept, but the configuration is deleted.`))
            call(() => api(`/printers/${id}`, { method: "DELETE" }), `Removed ${id}`);
          return;
        case "configure": return openDialog(p);
      }
    });
  });
}

$("#btn-reconnect-all").addEventListener("click", () =>
  call(() => api("/printers/reconnect-all", { method: "POST" }), "Reconnecting all printers"));
$("#btn-open-bt").addEventListener("click", () =>
  call(() => api("/system/open-bluetooth-settings", { method: "POST" })));

// ---- add/configure dialog ----
let editingID = null;
async function loadCandidates(selected) {
  const sel = $("#f-endpoint");
  sel.innerHTML = "<option>loading…</option>";
  try {
    const cands = (await api("/bluetooth/candidates")) || [];
    sel.innerHTML = cands.map((c) => {
      const label = c.deviceName ? `${c.endpoint} — ${c.deviceName}${c.isPrinter ? " 🖨" : ""}` : c.endpoint;
      return `<option value="${esc(c.endpoint)}" data-addr="${esc(c.deviceAddress || "")}">${esc(label)}</option>`;
    }).join("") || "<option value=''>no endpoints found</option>";
    if (selected) sel.value = selected;
  } catch (e) {
    sel.innerHTML = `<option value=''>error: ${esc(e.message)}</option>`;
  }
}

function openDialog(printer, selectedEndpoint = "") {
  editingID = printer?.id || null;
  $("#dialog-title").textContent = printer ? `Configure ${printer.id}` : "Add printer";
  $("#f-id").value = printer?.id || "";
  $("#f-id").disabled = !!printer;
  $("#f-name").value = printer?.displayName || "";
  $("#f-encoding").value = printer?.encoding || "CP858";
  $("#f-width").value = printer?.charactersPerLine || 32;
  loadCandidates(printer?.endpoint || selectedEndpoint);
  $("#printer-dialog").showModal();
}
$("#btn-add").addEventListener("click", () => openDialog(null));
$("#btn-refresh-candidates").addEventListener("click", () => loadCandidates($("#f-endpoint").value));
$("#btn-dialog-cancel").addEventListener("click", () => $("#printer-dialog").close());

$("#printer-form").addEventListener("submit", (ev) => {
  ev.preventDefault();
  const endpointSel = $("#f-endpoint");
  const body = {
    id: $("#f-id").value.trim(),
    displayName: $("#f-name").value.trim() || $("#f-id").value.trim(),
    endpoint: endpointSel.value,
    deviceAddress: endpointSel.selectedOptions[0]?.dataset.addr || "",
    encoding: $("#f-encoding").value,
    charactersPerLine: parseInt($("#f-width").value, 10) || 32,
  };
  const req = editingID
    ? api(`/printers/${editingID}`, { method: "PUT", body: JSON.stringify(body) })
    : api("/printers", { method: "POST", body: JSON.stringify(body) });
  call(() => req.then(() => $("#printer-dialog").close()), "Printer saved");
});

// ---- queue ----
async function refreshQueue() {
  if ($("#tab-queue").classList.contains("hidden")) return;
  const wantedStatus = $("#queue-filter").value;
  const selectedPrinter = $("#queue-printer").value;
  const printerIDs = selectedPrinter ? [selectedPrinter] : knownPrinters.map((st) => st.printer.id);
  const queues = await Promise.all(printerIDs.map((id) => api(`/printers/${encodeURIComponent(id)}/queue?limit=100`).catch(() => null)));
  let rows = [];
  for (const queue of queues.filter(Boolean)) {
    rows.push(...(queue.processingRun ? [queue.processingRun] : []), ...(queue.queuedRuns || []),
      ...(queue.retryPendingRuns || []), ...(queue.attentionRuns || []), ...(queue.recentTransmittedRuns || []));
  }
  const seen = new Set();
  rows = rows.filter((run) => !seen.has(run.uid) && seen.add(run.uid));
  if (wantedStatus) rows = rows.filter((run) => run.status === wantedStatus);
  $("#queue-table tbody").innerHTML = rows.map((d) => {
    const actions = [];
    if (d.status === "uncertain" && !d.resolution)
      actions.push(`<button data-act="confirm">It printed</button>`);
    if (d.status === "queued")
      actions.push(`<button data-act="cancel" class="danger">Cancel</button>`);
    actions.push(`<button data-act="job">View Job</button>`);
    return `<tr data-run-uid="${esc(d.uid)}" data-job-uid="${esc(d.jobUid)}">
      <td>${esc(d.uid)}${d.trigger === "manual_reprint" ? ' <span class="badge printing">reprint</span>' : ""}</td>
      <td>${esc(d.jobUid)}</td><td>${esc(d.printerId)}</td><td>${d.runNumber}</td><td>${esc(d.trigger)}</td>
      <td><span class="badge ${esc(d.status)}">${esc(d.status)}</span>${d.retryPending ? ' <span class="muted">retry pending</span>' : ""}</td>
      <td class="muted">${esc(d.errorMessage || "")}</td>
      <td class="row">${actions.join("")}</td>
    </tr>`;
  }).join("") || `<tr><td colspan="8" class="muted">No Print Runs</td></tr>`;

  $("#queue-table").querySelectorAll("button[data-act]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const row = btn.closest("tr");
      const runUID = encodeURIComponent(row.dataset.runUid);
      const jobUID = row.dataset.jobUid;
      const act = btn.dataset.act;
      if (act === "job") return openJob(jobUID);
      if (act === "cancel" && !confirm("Cancel this queued Print Run?")) return;
      if (act === "confirm" && !confirm("Confirm that paper physically came out?")) return;
      const path = act === "confirm" ? `/print-runs/${runUID}/confirm-printed` : `/print-runs/${runUID}/cancel`;
      call(() => api(path, { method: "POST" }), `${act} ok`);
    });
  });
}
$("#btn-queue-refresh").addEventListener("click", refreshQueue);
$("#queue-filter").addEventListener("change", refreshQueue);
$("#queue-printer").addEventListener("change", refreshQueue);

// ---- Dev / POS Simulator ----
let devLastRequest = null;
let devLastJobUID = "";
let devActiveJob = null;
let devLastReprint = null;
let devRefreshPending = false;

function devID(prefix) {
  return prefix + (crypto.randomUUID?.() || `${Date.now()}-${Math.random().toString(16).slice(2)}`);
}

function devSampleTicket() {
  return {
    orderNumber: `TEST-${new Date().toISOString().slice(11, 19).replaceAll(":", "")}`,
    createdAt: new Date().toISOString(),
    items: [
      { name: "Tortilla española", quantity: 2, notes: "Una sin cebolla" },
      { name: "Patatas bravas", quantity: 1 },
      { name: "Café con leche", quantity: 1, notes: "Sin azúcar" },
    ],
  };
}

function setDevResult(label, value, isError = false) {
  const result = $("#dev-result");
  result.textContent = `${label}\n${typeof value === "string" ? value : JSON.stringify(value, null, 2)}`;
  result.classList.toggle("error", isError);
}

function setDevSubmitStatus(message, isError = false) {
  const status = $("#dev-submit-status");
  status.textContent = message;
  status.classList.toggle("error", isError);
}

function newDevJobID() {
  $("#dev-job-id").value = devID("dashboard:kitchen:");
}

function newDevReprintID() {
  $("#dev-reprint-id").value = devID("dashboard:reprint:");
}

function resetDevTicket() {
  $("#dev-job-data").value = JSON.stringify(devSampleTicket(), null, 2);
  setDevSubmitStatus("Sample kitchen ticket loaded.");
}

function selectedDevPrinterIDs() {
  return [...document.querySelectorAll("[data-dev-printer]:checked")].map((input) => input.value);
}

function renderDevPrinters(list) {
  const root = $("#dev-printer-list");
  if (!root) return;
  const selected = new Set(selectedDevPrinterIDs());
  if (!list.length) {
    root.innerHTML = `<span class="muted">No printers configured. Add a printer from Setup first.</span>`;
    return;
  }
  const defaultIndex = Math.max(0, list.findIndex((status) => status.printer.enabled));
  root.innerHTML = list.map((status, index) => {
    const printer = status.printer;
    const checked = selected.has(printer.id) || (!selected.size && index === defaultIndex);
    return `<label class="dev-printer-choice">
      <input type="checkbox" data-dev-printer value="${esc(printer.id)}" ${checked ? "checked" : ""}>
      <span><b>${esc(printer.displayName || printer.id)}</b> <span class="badge ${esc(status.state)}">${esc(status.state)}</span>
      <small>${esc(printer.id)} · queue ${status.queueDepth}${status.attentionCount ? ` · ${status.attentionCount} need attention` : ""}</small></span>
    </label>`;
  }).join("");
}

function buildDevJobRequest() {
  const jobId = $("#dev-job-id").value.trim();
  if (!jobId) throw new Error("Job ID is required");
  const printerIds = selectedDevPrinterIDs();
  if (!printerIds.length) throw new Error("Select at least one printer");
  let data;
  try {
    data = JSON.parse($("#dev-job-data").value);
  } catch (error) {
    throw new Error(`Ticket data is not valid JSON: ${error.message}`);
  }
  return { jobId, template: "kitchen-ticket", data, printerIds };
}

function setDevActiveJob(job) {
  devActiveJob = job;
  $("#dev-job-card").classList.remove("hidden");
  $("#btn-dev-repeat-reprint").disabled = !devLastReprint || devLastReprint.jobUID !== job.uid;
  renderDevJob(job);
}

function renderDevJob(job) {
  $("#dev-job-summary").innerHTML = `<b>${esc(job.jobId)}</b> · <code>${esc(job.uid)}</code> ·
    <span class="badge ${esc(job.state)}">${esc(job.state)}</span> ·
    ${job.fulfilledPrinterCount}/${job.originalPrinterCount} fulfilled`;
  $("#dev-job-detail").innerHTML = (job.originalPrinters || []).map((target) => {
    const state = target.fulfilled ? "fulfilled" : target.cancelled ? "cancelled" : "unfulfilled";
    return `<div class="dev-target">
      <div class="dev-target-heading">
        <label><input type="checkbox" data-dev-reprint-target value="${esc(target.printerId)}"> <b>${esc(target.printerId)}</b></label>
        <span class="badge ${target.fulfilled ? "connected" : target.cancelled ? "disabled" : "error"}">${state}</span>
      </div>
      <table><thead><tr><th>#</th><th>Run UID</th><th>Trigger</th><th>Status</th><th>Bytes</th><th>Error</th></tr></thead><tbody>
        ${(target.runs || []).map((run) => `<tr><td>${run.runNumber}</td><td>${esc(run.uid)}</td>
          <td>${esc(run.trigger)}</td><td><span class="badge ${esc(run.status)}">${esc(run.status)}</span>${run.retryPending ? " · retry pending" : ""}</td>
          <td>${run.bytesAccepted}</td><td>${esc(run.errorMessage || "")}</td></tr>`).join("") ||
          `<tr><td colspan="6" class="muted">No Print Runs</td></tr>`}
      </tbody></table>
    </div>`;
  }).join("");
}

async function refreshDevJob(silent = false) {
  if (!devActiveJob || devRefreshPending || $("#tab-dev").classList.contains("hidden")) return;
  devRefreshPending = true;
  try {
    setDevActiveJob(await api(`/jobs/${encodeURIComponent(devActiveJob.uid)}`));
  } catch (error) {
    if (!silent) toast(error.message, true);
  } finally {
    devRefreshPending = false;
  }
}

function refreshDevSimulator() {
  renderDevPrinters(knownPrinters);
  refreshDevJob(true);
}

async function submitDevJob(request, mode) {
  try {
    const result = await api("/jobs", { method: "POST", body: JSON.stringify(request) });
    setDevResult(`POST /api/v1/jobs — ${result.duplicate ? "duplicate" : "accepted"}`, result);
    if (mode === "conflict") {
      setDevSubmitStatus("Conflict probe was unexpectedly accepted and may have printed", true);
      toast("Conflict probe was unexpectedly accepted", true);
    } else {
      devLastRequest = structuredClone(request);
      devLastJobUID = result.uid;
      $("#btn-dev-duplicate").disabled = false;
      $("#btn-dev-conflict").disabled = false;
      setDevSubmitStatus(result.duplicate ? "Duplicate request returned the existing job; nothing new was queued." : "Kitchen ticket accepted and queued.");
      toast(result.duplicate ? "Duplicate request deduplicated" : "Kitchen ticket queued");
    }
    setDevActiveJob(result);
    refreshJobs();
    refreshQueue();
  } catch (error) {
    const expected = mode === "conflict" && error.status === 409;
    setDevResult(`POST /api/v1/jobs — ${error.status || "error"}`, { code: error.code, error: error.message }, !expected);
    setDevSubmitStatus(expected ? `Expected idempotency conflict: ${error.message}` : error.message, !expected);
    toast(expected ? "Idempotency conflict verified" : error.message, !expected);
  }
}

async function devOriginalJobExists() {
  if (!devLastJobUID) return false;
  try {
    await api(`/jobs/${encodeURIComponent(devLastJobUID)}`);
    return true;
  } catch (error) {
    setDevSubmitStatus("The original job is no longer available, so the idempotency probe was not sent; it could print again.", true);
    toast(error.message, true);
    return false;
  }
}

async function submitNewDevJob() {
  let request;
  try {
    request = buildDevJobRequest();
  } catch (error) {
    setDevSubmitStatus(error.message, true);
    return toast(error.message, true);
  }
  if (!confirm(`Submit this kitchen ticket to ${request.printerIds.join(", ")}? This may produce physical output.`)) return;
  submitDevJob(request, "new");
}

async function duplicateDevJob() {
  if (!devLastRequest || !await devOriginalJobExists()) return;
  submitDevJob(structuredClone(devLastRequest), "duplicate");
}

async function conflictDevJob() {
  if (!devLastRequest || !await devOriginalJobExists()) return;
  const changed = structuredClone(devLastRequest);
  changed.data.orderNumber = `${changed.data.orderNumber}-CHANGED`;
  submitDevJob(changed, "conflict");
}

function selectedDevReprintTargets() {
  return [...document.querySelectorAll("[data-dev-reprint-target]:checked")].map((input) => input.value);
}

async function sendDevReprint(request) {
  try {
    const jobUID = devActiveJob.uid;
    const result = await api(`/jobs/${encodeURIComponent(jobUID)}/reprint`, {
      method: "POST", body: JSON.stringify(request),
    });
    devLastReprint = { jobUID, request: structuredClone(request) };
    $("#btn-dev-repeat-reprint").disabled = false;
    setDevResult(`POST /api/v1/jobs/${jobUID}/reprint — ${result.duplicate ? "duplicate" : "accepted"}`, result);
    toast(result.duplicate ? "Duplicate reprint request deduplicated" : "Reprint queued");
    await refreshDevJob();
    refreshJobs();
    refreshQueue();
  } catch (error) {
    setDevResult("Reprint failed", { code: error.code, error: error.message }, true);
    toast(error.message, true);
  }
}

function createDevReprint() {
  if (!devActiveJob) return;
  const printerIds = selectedDevReprintTargets();
  if (!printerIds.length) return toast("Select at least one active-job target", true);
  const reprintRequestId = $("#dev-reprint-id").value.trim();
  if (!reprintRequestId) return toast("Reprint request ID is required", true);
  const uncertain = (devActiveJob.originalPrinters || []).some((target) => printerIds.includes(target.printerId) &&
    (target.runs || []).some((run) => run.status === "uncertain" && !run.resolution));
  const warning = uncertain ? "An earlier result is uncertain and may already have printed. " : "";
  if (!confirm(`${warning}Create ${printerIds.length} physical reprint(s)?`)) return;
  sendDevReprint({ reprintRequestId, printerIds, reason: $("#dev-reprint-reason").value.trim() });
}

function repeatDevReprint() {
  if (!devLastReprint || !devActiveJob || devLastReprint.jobUID !== devActiveJob.uid) return;
  sendDevReprint(structuredClone(devLastReprint.request));
}

newDevJobID();
newDevReprintID();
resetDevTicket();
$("#btn-dev-new-job-id").addEventListener("click", newDevJobID);
$("#btn-dev-new-reprint-id").addEventListener("click", newDevReprintID);
$("#btn-dev-reset").addEventListener("click", resetDevTicket);
$("#btn-dev-submit").addEventListener("click", submitNewDevJob);
$("#btn-dev-duplicate").addEventListener("click", duplicateDevJob);
$("#btn-dev-conflict").addEventListener("click", conflictDevJob);
$("#btn-dev-refresh-job").addEventListener("click", () => refreshDevJob());
$("#btn-dev-open-job").addEventListener("click", () => { if (devActiveJob) openJob(devActiveJob.uid); });
$("#btn-dev-reprint").addEventListener("click", createDevReprint);
$("#btn-dev-repeat-reprint").addEventListener("click", repeatDevReprint);
$("#btn-dev-clear-result").addEventListener("click", () => {
  $("#dev-result").textContent = "No requests yet.";
  $("#dev-result").classList.remove("error");
});

// ---- pairing ----
async function refreshPairing() {
  const settings = await api("/admin/settings").catch(() => ({}));
  $("#allowed-origin").value = settings.allowedOrigin || "";
  const tokens = (await api("/admin/tokens").catch(() => [])) || [];
  $("#token-table tbody").innerHTML = tokens.map((t) => `<tr data-id="${esc(t.id)}">
      <td>${esc(t.label)}</td><td>${esc(t.allowedOrigin || "—")}</td>
      <td>${fmtTime(t.createdAt)}</td><td>${fmtTime(t.lastUsedAt)}</td>
      <td>${t.revoked ? '<span class="muted">revoked</span>' : '<button class="danger" data-act="revoke">Revoke</button>'}</td>
    </tr>`).join("") || `<tr><td colspan="5" class="muted">No tokens issued</td></tr>`;
  $("#token-table").querySelectorAll("button[data-act=revoke]").forEach((btn) =>
    btn.addEventListener("click", () => {
      const id = btn.closest("tr").dataset.id;
      call(() => api(`/admin/tokens/${id}`, { method: "DELETE" }).then(refreshPairing), "Token revoked");
    }));
}
$("#btn-save-origin").addEventListener("click", () =>
  call(() => api("/admin/settings", { method: "PUT", body: JSON.stringify({ allowedOrigin: $("#allowed-origin").value.trim() }) }), "Origin saved"));
$("#btn-pairing-code").addEventListener("click", async () => {
  try {
    const res = await api("/admin/pairing-code", { method: "POST" });
    const el = $("#pairing-code");
    el.textContent = res.code;
    el.classList.remove("hidden");
    toast("Code valid for 5 minutes");
  } catch (e) { toast(e.message, true); }
});

// ---- diagnostics & logs ----
async function refreshDiagnostics() {
  const d = await api("/diagnostics").catch((e) => ({ error: e.message }));
  $("#diag-body").textContent = JSON.stringify(d, null, 2);
}
$("#btn-diag-refresh").addEventListener("click", refreshDiagnostics);

async function refreshLogs() {
  const rows = (await api("/logs?limit=200").catch(() => [])) || [];
  $("#log-table tbody").innerHTML = rows.map((e) => `<tr>
      <td class="muted">${fmtTime(e.createdAt)}</td><td>${esc(e.type)}</td>
      <td>${esc(e.printerId || "")}</td><td class="muted">${esc(e.runUid || "")}</td>
      <td>${esc(e.message || "")}</td>
    </tr>`).join("") || `<tr><td colspan="5" class="muted">No events</td></tr>`;
}
$("#btn-logs-refresh").addEventListener("click", refreshLogs);

// ---- polling ----
refreshStatus();
initializeNavigation();
setInterval(refreshStatus, 2000);
setInterval(refreshQueue, 5000);
setInterval(refreshBluetoothDevices, 2000);
setInterval(() => refreshDevJob(true), 2000);
