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
  "/admin/operations/printer-logs": { panel: "printer-logs", category: "operations", title: "Printer Logs" },
  "/admin/system/diagnostics": { panel: "diagnostics", category: "system", title: "Diagnostics" },
  "/admin/system/logs": { panel: "system-logs", category: "system", title: "System Logs" },
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
    "system-logs": refreshSystemLogs,
    "printer-logs": refreshPrinterLogs,
    dev: refreshDevSimulator,
  };
  refreshers[page.panel]?.();
  syncPrinterDetailFromURL();
}

function navigateDashboard(path, { replace = false } = {}) {
  let target = new URL(path, window.location.origin);
  if (!dashboardRoutes[target.pathname]) target = new URL(defaultDashboardPath, window.location.origin);
  const destination = target.pathname + target.search;
  if (window.location.pathname + window.location.search !== destination) {
    window.history[replace ? "replaceState" : "pushState"]({}, "", destination);
  }
  closeNavigationMenus();
  activateDashboardPath(target.pathname);
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

function wireCheckboxGroup(master, root, itemSelector) {
  const items = () => [...root.querySelectorAll(itemSelector)].filter((input) => !input.disabled);
  const syncMaster = () => {
    const available = items();
    const selected = available.filter((input) => input.checked).length;
    master.disabled = available.length === 0;
    master.checked = available.length > 0 && selected === available.length;
    master.indeterminate = selected > 0 && selected < available.length;
  };
  master.onchange = () => {
    items().forEach((input) => { input.checked = master.checked; });
    syncMaster();
  };
  items().forEach((input) => { input.onchange = syncMaster; });
  syncMaster();
}

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

let activeJobDetail = null;
let jobDialogRequest = 0;
const jobRunSignatures = new WeakMap();

function jobTargetState(target) {
  if (target.fulfilled) return { label: "fulfilled", badge: "connected" };
  if (target.cancelled) return { label: "cancelled", badge: "cancelled" };
  return { label: "unfulfilled", badge: "error" };
}

function jobRunRowsHTML(target) {
  return (target.runs || []).map((run) => `<tr><td>${run.runNumber}</td><td>${esc(run.uid)}</td>
    <td>${esc(run.trigger)}</td><td>${esc(run.status)}${run.retryPending ? " · retry pending" : ""}</td>
    <td>${esc(run.errorMessage || "")}</td></tr>`).join("") ||
    `<tr><td colspan="5" class="muted">No Print Runs</td></tr>`;
}

function jobPrinterCardHTML(target, index) {
  const state = jobTargetState(target);
  return `<div class="card job-printer" data-printer-id="${esc(target.printerId)}">
    <h3><label class="checkbox-label" for="job-target-${index}"><input id="job-target-${index}" type="checkbox" data-reprint-printer value="${esc(target.printerId)}"> ${esc(target.printerId)}</label>
      <span data-job-target-state class="badge ${state.badge}">${state.label}</span></h3>
    <table><thead><tr><th>#</th><th>UID</th><th>Trigger</th><th>Status</th><th>Error</th></tr></thead>
      <tbody>${jobRunRowsHTML(target)}</tbody></table></div>`;
}

function jobDialogHTML(job) {
  const printerCards = (job.originalPrinters || []).map(jobPrinterCardHTML).join("");
  return `<div class="card">
    <div><b>UID:</b> <span data-job-summary="uid">${esc(job.uid)}</span></div>
    <div><b>State:</b> <span data-job-summary="state">${esc(job.state)}</span></div>
    <div><b>Original printers:</b> <span data-job-summary="printers">${job.fulfilledPrinterCount}/${job.originalPrinterCount} fulfilled</span></div>
    <pre>${esc(JSON.stringify(job.data, null, 2))}</pre></div>
    <div class="selection-toolbar"><label class="checkbox-label" for="job-select-all"><input id="job-select-all" type="checkbox"> Select all targets</label></div>
    ${printerCards}
    <div class="toolbar"><button id="btn-job-reprint" class="primary">Reprint selected</button>
      <button id="btn-job-cancel" class="danger">Cancel selected targets</button></div>`;
}

function updateJobTargetCard(card, target) {
  const state = jobTargetState(target);
  const badge = card.querySelector("[data-job-target-state]");
  badge.textContent = state.label;
  badge.className = `badge ${state.badge}`;
  const body = card.querySelector("tbody");
  const signature = JSON.stringify((target.runs || []).map((run) =>
    [run.uid, run.runNumber, run.trigger, run.status, run.retryPending, run.errorMessage]));
  if (jobRunSignatures.get(body) !== signature) {
    body.innerHTML = jobRunRowsHTML(target);
    jobRunSignatures.set(body, signature);
  }
}

function renderJobDialog(job) {
  const root = $("#job-detail");
  const sameJob = activeJobDetail?.uid === job.uid && root.dataset.jobUid === job.uid && $("#job-dialog").open;
  activeJobDetail = job;
  $("#job-dialog-title").textContent = `${job.jobId} / ${job.template}`;

  const targets = job.originalPrinters || [];
  const cards = [...root.querySelectorAll(".job-printer")];
  const targetsMatch = sameJob && cards.length === targets.length &&
    cards.every((card, index) => card.dataset.printerId === targets[index].printerId);
  if (!targetsMatch) {
    root.dataset.jobUid = job.uid;
    root.innerHTML = jobDialogHTML(job);
    wireCheckboxGroup($("#job-select-all"), root, "[data-reprint-printer]");
    $("#btn-job-reprint").addEventListener("click", reprintSelectedJobTargets);
    $("#btn-job-cancel").addEventListener("click", cancelSelectedJobTargets);
    [...root.querySelectorAll(".job-printer")].forEach((card, index) => updateJobTargetCard(card, targets[index]));
    return;
  }

  root.querySelector('[data-job-summary="state"]').textContent = job.state;
  root.querySelector('[data-job-summary="printers"]').textContent =
    `${job.fulfilledPrinterCount}/${job.originalPrinterCount} fulfilled`;
  cards.forEach((card, index) => updateJobTargetCard(card, targets[index]));
}

function selectedJobTargetIDs() {
  return [...$("#job-detail").querySelectorAll("[data-reprint-printer]:checked")].map((input) => input.value);
}

function setJobActionsPending(pending) {
  [$("#btn-job-reprint"), $("#btn-job-cancel")].filter(Boolean).forEach((button) => { button.disabled = pending; });
}

async function refreshJobAfterAction(jobUID) {
  if ($("#job-dialog").open && activeJobDetail?.uid === jobUID) await openJob(jobUID, { show: false });
  refreshJobs();
  refreshQueue();
}

async function reprintSelectedJobTargets() {
  const job = activeJobDetail;
  if (!job) return;
  const printerIds = selectedJobTargetIDs();
  if (!printerIds.length) return toast("Select at least one printer", true);
  const uncertain = (job.originalPrinters || []).some((target) => printerIds.includes(target.printerId) &&
    (target.runs || []).some((run) => run.status === "uncertain" && !run.resolution));
  const warning = uncertain ? "The earlier result is uncertain and may already have printed. " : "";
  if (!confirm(`${warning}Create ${printerIds.length} physical reprint(s)?`)) return;
  const reason = prompt("Reason for reprint (optional):", "") || "";
  setJobActionsPending(true);
  try {
    await api(`/jobs/${encodeURIComponent(job.uid)}/reprint`, { method: "POST", body: JSON.stringify({
      reprintRequestId: crypto.randomUUID(), printerIds, reason,
    }) });
    toast("Reprint queued");
    await refreshJobAfterAction(job.uid);
  } catch (error) {
    toast(error.message, true);
  } finally {
    setJobActionsPending(false);
  }
}

async function cancelSelectedJobTargets() {
  const job = activeJobDetail;
  if (!job) return;
  const printerIds = selectedJobTargetIDs();
  if (!printerIds.length) return toast("Select at least one printer", true);
  if (!confirm(`Cancel pending output for ${printerIds.length} original printer target(s)?`)) return;
  const reason = prompt("Cancellation reason (optional):", "") || "";
  setJobActionsPending(true);
  try {
    await api(`/jobs/${encodeURIComponent(job.uid)}/cancel`, {
      method: "POST", body: JSON.stringify({ printerIds, reason }),
    });
    toast("Targets cancelled");
    await refreshJobAfterAction(job.uid);
  } catch (error) {
    toast(error.message, true);
  } finally {
    setJobActionsPending(false);
  }
}

async function openJob(uid, { show = true } = {}) {
  const request = ++jobDialogRequest;
  try {
    const job = await api(`/jobs/${encodeURIComponent(uid)}`);
    if (request !== jobDialogRequest) return;
    renderJobDialog(job);
    if (show && !$("#job-dialog").open) $("#job-dialog").showModal();
  } catch (error) {
    if (request === jobDialogRequest) toast(error.message, true);
  }
}

$("#btn-jobs-refresh").addEventListener("click", refreshJobs);
$("#jobs-filter").addEventListener("change", refreshJobs);
$("#btn-job-close").addEventListener("click", () => $("#job-dialog").close());
$("#job-dialog").addEventListener("close", () => {
  jobDialogRequest++;
  activeJobDetail = null;
});

// ---- Bluetooth discovery & device pairing ----
let scanning = false;
let scanTimer;
const pairingAddresses = new Set();
const selectedDeviceProtocols = new Map();
const enteredDevicePINs = new Map();

async function prepareProtocolForPairing(connectionType) {
  await stopBluetoothDiscovery(true);
  if (connectionType === "auto") return;
  await api(`/bluetooth/discovery/start?${new URLSearchParams({ connectionType })}`, { method: "POST" });
  scanning = true;
  await new Promise((resolve) => setTimeout(resolve, 2500));
  await api("/bluetooth/discovery/stop", { method: "POST" });
  scanning = false;
}

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
    const [result, printerStatuses] = await Promise.all([
      api("/bluetooth/devices"),
      api("/printers").catch(() => knownPrinters),
    ]);
    knownPrinters = printerStatuses || [];
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

function normalizeBluetoothAddress(value) {
  return String(value || "").trim().replaceAll("-", ":").toUpperCase();
}

function bluetoothAddressFromEndpoint(endpoint) {
  const value = String(endpoint || "").trim();
  const separator = value.indexOf("://");
  return separator >= 0 ? normalizeBluetoothAddress(value.slice(separator + 3)) : "";
}

function configuredPrintersForDevice(device) {
  const endpoint = String(device.endpoint || "").trim().toLowerCase();
  const address = normalizeBluetoothAddress(device.address);
  return knownPrinters.filter((status) => {
    const printer = status.printer || {};
    if (endpoint && String(printer.endpoint || "").trim().toLowerCase() === endpoint) return true;
    const configuredAddress = normalizeBluetoothAddress(printer.deviceAddress) || bluetoothAddressFromEndpoint(printer.endpoint);
    return address !== "" && configuredAddress === address;
  });
}

function activeConnectionType(status) {
  if (!status || !["connected", "printing"].includes(status.state)) return "none";
  return String(status.endpoint || "").split("://")[0] || "unknown";
}

function renderBluetoothDevices(devices) {
  const root = $("#device-list");
  if (!devices.length) {
    root.innerHTML = `<div class="card muted">${scanning ? "Looking for nearby Bluetooth devices…" : "No devices found yet. Select Scan for devices."}</div>`;
    return;
  }
  root.innerHTML = devices.map((device) => {
    const busy = pairingAddresses.has(device.address);
    const configured = configuredPrintersForDevice(device);
    const state = device.paired
      ? `<span class="badge connected">paired</span>`
      : device.endpoint
        ? `<span class="badge connected">ready without pairing</span>`
        : `<span class="badge disabled">not paired</span>`;
    const connected = device.connected ? `<span class="badge printing">connected</span>` : "";
    const printer = device.isPrinter ? `<span class="device-kind">🖨 Likely printer</span>` : "";
    const configuredNames = configured.map((status) => status.printer.displayName || status.printer.id).join(", ");
    const activeTypes = [...new Set(configured.map(activeConnectionType).filter((type) => type !== "none"))];
    const supported = device.supportedConnectionTypes || [];
    const selectedProtocol = selectedDeviceProtocols.get(device.address) || "";
    const enteredPIN = enteredDevicePINs.get(device.address) || "";
    const protocolOptions = [
      ["", "Choose protocol…"], ["auto", "Auto"], ["rfcomm", "RFCOMM (Classic)"], ["ble", "BLE"],
    ].map(([value, label]) => `<option value="${value}" ${value === selectedProtocol ? "selected" : ""} ${value && value !== "auto" && !supported.includes(value) ? "disabled" : ""}>${label}</option>`).join("");
    const actions = [];
    if (configured.length) {
      actions.push(`<span class="configured-device"><span class="badge connected">added</span> ${esc(configuredNames)}</span>`);
      actions.push(`<button data-device-act="manage" data-printer-id="${esc(configured[0].printer.id)}">View printer</button>`);
    } else {
      if (!device.paired) actions.push(`<select data-device-protocol required>${protocolOptions}</select>
        <input class="pin-input" data-device-pin value="${esc(enteredPIN)}" placeholder="PIN if required" inputmode="numeric" maxlength="16">
        <button class="primary" data-device-act="pair" ${busy ? "disabled" : ""}>${busy ? "Pairing…" : "Pair"}</button>`);
      if (device.endpoint) actions.push(`<button class="primary" data-device-act="add" data-endpoint="${esc(device.endpoint)}">Add printer</button>`);
      if (device.paired) actions.push(`<button class="danger" data-device-act="forget">Forget</button>`);
    }
    if (device.connected) actions.push(`<button data-device-act="disconnect">Disconnect now</button>`);
    const action = actions.join("");
    return `<div class="card device" data-address="${esc(device.address)}">
      <div class="device-info">
        <h2>${esc(device.name || "Unknown device")} ${state} ${connected}</h2>
        <div class="meta">${esc(device.address)} ${printer}${activeTypes.length ? ` · active: <b>${esc(activeTypes.join(", "))}</b>` : ""}</div>
      </div>
      <div class="device-actions">${action}</div>
    </div>`;
  }).join("");

  root.querySelectorAll("select[data-device-protocol]").forEach((select) => {
    select.addEventListener("change", () => {
      selectedDeviceProtocols.set(select.closest(".device").dataset.address, select.value);
    });
  });
  root.querySelectorAll("input[data-device-pin]").forEach((input) => {
    input.addEventListener("input", () => {
      enteredDevicePINs.set(input.closest(".device").dataset.address, input.value);
    });
  });

  root.querySelectorAll("button[data-device-act=pair]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const card = btn.closest(".device");
      const address = card.dataset.address;
      const pin = card.querySelector("[data-device-pin]").value.trim();
      const connectionType = card.querySelector("[data-device-protocol]").value;
      if (!connectionType) { toast("Choose Auto, RFCOMM, or BLE before pairing", true); return; }
      pairingAddresses.add(address);
      renderBluetoothDevices(devices);
      try {
        await prepareProtocolForPairing(connectionType);
        const result = await api(`/bluetooth/devices/${encodeURIComponent(address)}/pair`, {
          method: "POST",
          body: JSON.stringify({ pin, connectionType }),
        });
        toast(result.ready
          ? `${address} is ready. You can add it as a printer.`
          : `Paired ${address}, but no supported printer endpoint was found.`);
        selectedDeviceProtocols.delete(address);
        enteredDevicePINs.delete(address);
      } catch (e) {
        toast(e.message, true);
      } finally {
        pairingAddresses.delete(address);
        refreshBluetoothDevices();
      }
    });
  });
  root.querySelectorAll("button[data-device-act=disconnect]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const address = btn.closest(".device").dataset.address;
      if (!confirm("Disconnect this device now? Auto-reconnect may connect it again immediately.")) return;
      await call(() => api(`/bluetooth/devices/${encodeURIComponent(address)}/disconnect`, { method: "POST" }), `Disconnected ${address}`);
      refreshBluetoothDevices();
    });
  });
  root.querySelectorAll("button[data-device-act=forget]").forEach((btn) => {
    btn.addEventListener("click", async () => {
      const address = btn.closest(".device").dataset.address;
      if (!confirm(`Forget ${address}? It will need to be paired again.`)) return;
      await call(() => api(`/bluetooth/devices/${encodeURIComponent(address)}`, { method: "DELETE" }), `Forgot ${address}`);
      refreshBluetoothDevices();
    });
  });
  root.querySelectorAll("button[data-device-act=add]").forEach((btn) => {
    btn.addEventListener("click", () => {
      activateTab("printers");
      openDialog(null, btn.dataset.endpoint);
    });
  });
  root.querySelectorAll("button[data-device-act=manage]").forEach((btn) => {
    btn.addEventListener("click", () => {
      navigateDashboard(`/admin/setup/printers?${new URLSearchParams({ printerId: btn.dataset.printerId })}`);
    });
  });
}

async function startBluetoothDiscovery() {
  clearTimeout(scanTimer);
  try {
    const connectionType = $("#scan-connection-type").value || "auto";
    await api(`/bluetooth/discovery/start?${new URLSearchParams({ connectionType })}`, { method: "POST" });
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
$("#btn-copy-ubuntu-command").addEventListener("click", async () => {
  await navigator.clipboard.writeText($("#ubuntu-stop-command").textContent);
  toast("Ubuntu command copied");
});

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
let knownPrintersLoaded = false;
function renderPrinters(list) {
	knownPrinters = list;
	knownPrintersLoaded = true;
	renderDevPrinters(list);
	const requestedQueuePrinter = window.location.pathname === "/admin/operations/queue"
		? new URLSearchParams(window.location.search).get("printerId") || ""
		: "";
	const selected = requestedQueuePrinter || $("#queue-printer")?.value || "";
	if ($("#queue-printer")) {
		$("#queue-printer").innerHTML = `<option value="">All printers</option>` + list.map((st) =>
			`<option value="${esc(st.printer.id)}">${esc(st.printer.displayName)}</option>`).join("");
		$("#queue-printer").value = selected;
	}
	refreshPrinterLogPrinterOptions();
  const root = $("#printer-list");
  if (!list.length) {
    root.innerHTML = `<div class="card muted">No printers configured yet. Pair devices in the first tab, then use <b>Add printer</b>.</div>`;
    syncPrinterDetailFromURL();
    return;
  }
  root.innerHTML = list.map((st) => {
    const p = st.printer;
    const attn = st.attentionCount ? `<span class="attn">⚠ ${st.attentionCount} need attention</span>` : "";
    const retry = st.nextRetryAt ? ` · retry ${fmtTime(st.nextRetryAt)} (attempt ${st.reconnectAttempt})` : "";
    return `<div class="card printer" data-id="${esc(p.id)}" role="button" tabindex="0" aria-label="Open ${esc(p.displayName)} details">
      <div>
        <h2>${esc(p.displayName)} <span class="badge ${esc(st.state)}">${esc(st.state)}</span></h2>
        <div class="meta"><b>${esc(st.endpoint || "no endpoint")}</b> · active: <b>${esc(activeConnectionType(st))}</b> · queue: <b>${st.queueDepth}</b> ·
          last transmission: ${fmtTime(st.lastTransmission)}${retry} ${attn}</div>
        ${st.lastError ? `<div class="error">${esc(st.lastError)}</div>` : ""}
      </div>
      <div class="actions">
        <button data-act="test">Test print</button>
        <button data-act="reconnect">Reconnect</button>
      </div>
    </div>`;
  }).join("");

  root.querySelectorAll("button[data-act]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const id = btn.closest(".printer").dataset.id;
      switch (btn.dataset.act) {
        case "test": return call(() => api(`/printers/${id}/test`, { method: "POST" }), `Test print queued for ${id}`);
        case "reconnect": return call(() => api(`/printers/${id}/reconnect`, { method: "POST" }), `Reconnecting ${id}`);
      }
    });
  });
  root.querySelectorAll(".printer").forEach((card) => {
    const open = () => navigateDashboard(printerRoute(card.dataset.id));
    card.addEventListener("click", (event) => { if (!event.target.closest("button")) open(); });
    card.addEventListener("keydown", (event) => {
      if (event.target !== card || (event.key !== "Enter" && event.key !== " ")) return;
      event.preventDefault();
      open();
    });
  });
  syncPrinterDetailFromURL();
}

// ---- printer details ----
let printerDetailID = "";
let printerDetailTab = "overview";
let printerDetailQueueRequest = 0;
let printerDetailLogsRequest = 0;

function printerStatus(id = printerDetailID) {
  return knownPrinters.find((status) => status.printer.id === id) || null;
}

function printerRoute(id) {
  return `/admin/setup/printers?${new URLSearchParams({ printerId: id })}`;
}

function hidePrinterDetail() {
  printerDetailID = "";
  printerDetailQueueRequest++;
  printerDetailLogsRequest++;
  if ($("#printer-detail-dialog").open) $("#printer-detail-dialog").close();
}

function printerInfoItem(label, value) {
  return `<div class="printer-info-item"><dt>${esc(label)}</dt><dd>${esc(value ?? "—")}</dd></div>`;
}

function detailDate(value) {
  return value ? new Date(value).toLocaleString() : "—";
}

function renderPrinterDetailOverview(status) {
  const printer = status.printer;
  $("#printer-detail-title").textContent = printer.displayName;
  $("#printer-detail-state").textContent = status.state;
  $("#printer-detail-state").className = `badge ${status.state}`;
  $("#printer-detail-subtitle").textContent = `${printer.id} · ${status.endpoint || "No endpoint"}`;
  $("#printer-detail-info").innerHTML = [
    ["Printer ID", printer.id],
    ["Connection", status.state],
    ["Enabled", printer.enabled ? "Yes" : "No"],
    ["Endpoint", status.endpoint || printer.endpoint || "—"],
    ["Active connection type", activeConnectionType(status)],
    ["Default connection type", printer.connectionPreference || "auto"],
    ["Device address", printer.deviceAddress || "—"],
    ["Transport", printer.transport || "—"],
    ["Queue depth", status.queueDepth],
    ["Needs attention", status.attentionCount],
    ["Last transmission", detailDate(status.lastTransmission)],
    ["Next reconnect", status.nextRetryAt ? `${detailDate(status.nextRetryAt)} (attempt ${status.reconnectAttempt || 1})` : "—"],
    ["Encoding", printer.encoding || "—"],
    ["Characters per line", printer.charactersPerLine || "—"],
    ["Serial settings", printer.baudRate ? `${printer.baudRate} baud · ${printer.dataBits || 8}/${printer.parity || "none"}/${printer.stopBits || 1}` : "—"],
    ["Auto reconnect", printer.autoReconnect ? "Yes" : "No"],
    ["Updated", detailDate(printer.updatedAt)],
  ].map(([label, value]) => printerInfoItem(label, value)).join("");
  const error = $("#printer-detail-error");
  error.textContent = status.lastError || "";
  error.classList.toggle("hidden", !status.lastError);
  $("#btn-printer-detail-toggle").textContent = printer.enabled ? "Disable" : "Enable";
}

function syncPrinterDetailFromURL() {
  const dialog = $("#printer-detail-dialog");
  if (!dialog) return;
  const requestedID = window.location.pathname === "/admin/setup/printers"
    ? new URLSearchParams(window.location.search).get("printerId") || ""
    : "";
  if (!requestedID) {
    hidePrinterDetail();
    return;
  }
  if (!knownPrintersLoaded) return;
  const status = printerStatus(requestedID);
  if (!status) {
    hidePrinterDetail();
    navigateDashboard("/admin/setup/printers", { replace: true });
    toast(`Printer ${requestedID} was not found`, true);
    return;
  }
  const changedPrinter = printerDetailID !== requestedID;
  printerDetailID = requestedID;
  if (changedPrinter) printerDetailTab = "overview";
  renderPrinterDetailOverview(status);
  if (!dialog.open) dialog.showModal();
  setPrinterDetailTab(printerDetailTab, { refresh: changedPrinter });
}

function setPrinterDetailTab(tab, { refresh = true } = {}) {
  if (!["overview", "queue", "logs"].includes(tab)) tab = "overview";
  printerDetailTab = tab;
  document.querySelectorAll("[data-printer-detail-tab]").forEach((button) => {
    const active = button.dataset.printerDetailTab === tab;
    button.setAttribute("aria-selected", String(active));
    button.tabIndex = active ? 0 : -1;
  });
  for (const name of ["overview", "queue", "logs"]) {
    $("#printer-detail-" + name).classList.toggle("hidden", name !== tab);
  }
  if (refresh && tab === "queue") refreshPrinterDetailQueue();
  if (refresh && tab === "logs") refreshPrinterDetailLogs();
}

function collectQueueRuns(queue) {
  const runs = [
    ...(queue.processingRun ? [queue.processingRun] : []),
    ...(queue.queuedRuns || []),
    ...(queue.retryPendingRuns || []),
    ...(queue.attentionRuns || []),
    ...(queue.recentTransmittedRuns || []),
  ];
  const seen = new Set();
  return runs.filter((run) => !seen.has(run.uid) && seen.add(run.uid));
}

async function refreshPrinterDetailQueue() {
  const id = printerDetailID;
  if (!id || printerDetailTab !== "queue") return;
  const request = ++printerDetailQueueRequest;
  $("#printer-detail-queue-body").innerHTML = `<span class="muted">Loading recent Print Runs…</span>`;
  try {
    const queue = await api(`/printers/${encodeURIComponent(id)}/queue?limit=50`);
    if (request !== printerDetailQueueRequest || id !== printerDetailID) return;
    const rows = collectQueueRuns(queue);
    $("#printer-detail-queue-body").innerHTML = `<table><thead><tr><th>Created</th><th>Run</th><th>Job</th><th>#</th><th>Trigger</th><th>Status</th><th>Error</th><th></th></tr></thead><tbody>${rows.map((run) => `<tr data-job-uid="${esc(run.jobUid)}">
      <td class="muted">${esc(detailDate(run.createdAt))}</td><td>${esc(run.uid)}</td><td>${esc(run.jobUid)}</td><td>${esc(run.runNumber)}</td>
      <td>${esc(run.trigger)}</td><td><span class="badge ${esc(run.status)}">${esc(run.status)}</span></td>
      <td>${esc(run.errorMessage || "")}</td><td><button data-view-job>View Job</button></td></tr>`).join("") || `<tr><td colspan="8" class="muted">No Print Runs for this printer</td></tr>`}</tbody></table>`;
    $("#printer-detail-queue-body").querySelectorAll("button[data-view-job]").forEach((button) => {
      button.addEventListener("click", () => openJob(button.closest("tr").dataset.jobUid));
    });
  } catch (error) {
    if (request === printerDetailQueueRequest && id === printerDetailID)
      $("#printer-detail-queue-body").innerHTML = `<div class="error">${esc(error.message)}</div>`;
  }
}

async function refreshPrinterDetailLogs() {
  const id = printerDetailID;
  if (!id || printerDetailTab !== "logs") return;
  const request = ++printerDetailLogsRequest;
  $("#printer-detail-logs-body").innerHTML = `<span class="muted">Loading recent printer events…</span>`;
  try {
    const result = await fetchPrinterLogEvents({ filters: { printerId: id }, limit: 20 });
    if (request !== printerDetailLogsRequest || id !== printerDetailID) return;
    const entries = result.events || [];
    $("#printer-detail-logs-body").innerHTML = `<table><thead><tr><th>Time</th><th>Event</th><th>Run</th><th>Message</th></tr></thead><tbody>${printerLogRowsHTML(entries, {
      emptyMessage: "No recent events for this printer",
    })}</tbody></table>`;
  } catch (error) {
    if (request === printerDetailLogsRequest && id === printerDetailID)
      $("#printer-detail-logs-body").innerHTML = `<div class="error">${esc(error.message)}</div>`;
  }
}

document.querySelectorAll("[data-printer-detail-tab]").forEach((button) => {
  button.addEventListener("click", () => setPrinterDetailTab(button.dataset.printerDetailTab));
});
$("#btn-printer-detail-close").addEventListener("click", () => $("#printer-detail-dialog").close());
$("#printer-detail-dialog").addEventListener("close", () => {
  if (!printerDetailID) return;
  printerDetailID = "";
  if (window.location.pathname === "/admin/setup/printers")
    navigateDashboard("/admin/setup/printers", { replace: true });
});
$("#btn-printer-detail-test").addEventListener("click", () => {
  const id = printerDetailID;
  if (id) call(() => api(`/printers/${encodeURIComponent(id)}/test`, { method: "POST" }), `Test print queued for ${id}`);
});
$("#btn-printer-detail-reconnect").addEventListener("click", () => {
  const id = printerDetailID;
  if (id) call(() => api(`/printers/${encodeURIComponent(id)}/reconnect`, { method: "POST" }), `Reconnecting ${id}`);
});
$("#btn-printer-detail-configure").addEventListener("click", () => {
  const status = printerStatus();
  if (status) openDialog(status.printer);
});
$("#btn-printer-detail-toggle").addEventListener("click", () => {
  const status = printerStatus();
  if (!status) return;
  const operation = status.printer.enabled ? "disable" : "enable";
  call(() => api(`/printers/${encodeURIComponent(printerDetailID)}/${operation}`, { method: "POST" }),
    `${status.printer.displayName} ${operation}d`);
});
$("#btn-printer-detail-remove").addEventListener("click", async () => {
  const status = printerStatus();
  if (!status || !confirm(`Remove ${status.printer.displayName}? Existing job history will be preserved.`)) return;
  const id = printerDetailID;
  try {
    await api(`/printers/${encodeURIComponent(id)}`, { method: "DELETE" });
    hidePrinterDetail();
    navigateDashboard("/admin/setup/printers", { replace: true });
    toast(`${status.printer.displayName} removed`);
    refreshStatus();
  } catch (error) {
    toast(error.message, true);
  }
});
$("#btn-printer-detail-refresh-queue").addEventListener("click", refreshPrinterDetailQueue);
$("#btn-printer-detail-refresh-logs").addEventListener("click", refreshPrinterDetailLogs);
$("#btn-printer-detail-full-queue").addEventListener("click", () => {
  if (printerDetailID) navigateDashboard(`/admin/operations/queue?${new URLSearchParams({ printerId: printerDetailID })}`);
});
$("#btn-printer-detail-full-logs").addEventListener("click", () => {
  if (printerDetailID) navigateDashboard(`/admin/operations/printer-logs?${new URLSearchParams({ printerId: printerDetailID })}`);
});

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
  $("#f-connection-preference").value = printer?.connectionPreference || "";
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
    connectionPreference: $("#f-connection-preference").value,
  };
  const req = editingID
    ? api(`/printers/${editingID}`, { method: "PUT", body: JSON.stringify(body) })
    : api("/printers", { method: "POST", body: JSON.stringify(body) });
  call(() => req.then(() => $("#printer-dialog").close()), "Printer saved");
});

// ---- queue ----
function syncQueuePrinterFilter() {
  if (window.location.pathname !== "/admin/operations/queue") return;
  const requested = new URLSearchParams(window.location.search).get("printerId") || "";
  const select = $("#queue-printer");
  select.value = [...select.options].some((option) => option.value === requested) ? requested : "";
}

async function refreshQueue() {
  if ($("#tab-queue").classList.contains("hidden")) return;
  syncQueuePrinterFilter();
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
$("#queue-printer").addEventListener("change", () => {
  const params = new URLSearchParams();
  if ($("#queue-printer").value) params.set("printerId", $("#queue-printer").value);
  navigateDashboard(`/admin/operations/queue${params.size ? `?${params}` : ""}`);
});

// ---- Dev / POS Simulator ----
let devLastRequest = null;
let devLastJobUID = "";
let devActiveJob = null;
let devLastReprint = null;
let devRefreshPending = false;
let devPrinterSelectionInitialized = false;

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
    wireCheckboxGroup($("#dev-select-all-printers"), root, "[data-dev-printer]");
    return;
  }
  const defaultIndex = Math.max(0, list.findIndex((status) => status.printer.enabled));
  root.innerHTML = list.map((status, index) => {
    const printer = status.printer;
    const checked = selected.has(printer.id) || (!devPrinterSelectionInitialized && index === defaultIndex);
    return `<label class="dev-printer-choice" for="dev-printer-${index}">
      <input id="dev-printer-${index}" type="checkbox" data-dev-printer value="${esc(printer.id)}" ${checked ? "checked" : ""}>
      <span><b>${esc(printer.displayName || printer.id)}</b> <span class="badge ${esc(status.state)}">${esc(status.state)}</span>
      <small>${esc(printer.id)} · queue ${status.queueDepth}${status.attentionCount ? ` · ${status.attentionCount} need attention` : ""}</small></span>
    </label>`;
  }).join("");
  devPrinterSelectionInitialized = true;
  wireCheckboxGroup($("#dev-select-all-printers"), root, "[data-dev-printer]");
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
  const preserveSelection = devActiveJob?.uid === job.uid;
  devActiveJob = job;
  $("#dev-job-card").classList.remove("hidden");
  $("#btn-dev-repeat-reprint").disabled = !devLastReprint || devLastReprint.jobUID !== job.uid;
  renderDevJob(job, preserveSelection);
}

function renderDevJob(job, preserveSelection) {
  const selected = preserveSelection
    ? new Set([...document.querySelectorAll("[data-dev-reprint-target]:checked")].map((input) => input.value))
    : new Set();
  $("#dev-job-summary").innerHTML = `<b>${esc(job.jobId)}</b> · <code>${esc(job.uid)}</code> ·
    <span class="badge ${esc(job.state)}">${esc(job.state)}</span> ·
    ${job.fulfilledPrinterCount}/${job.originalPrinterCount} fulfilled`;
  const root = $("#dev-job-detail");
  root.innerHTML = (job.originalPrinters || []).map((target, index) => {
    const state = target.fulfilled ? "fulfilled" : target.cancelled ? "cancelled" : "unfulfilled";
    return `<div class="dev-target">
      <div class="dev-target-heading">
        <label class="checkbox-label" for="dev-target-${index}"><input id="dev-target-${index}" type="checkbox" data-dev-reprint-target value="${esc(target.printerId)}" ${selected.has(target.printerId) ? "checked" : ""}> <b>${esc(target.printerId)}</b></label>
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
  wireCheckboxGroup($("#dev-select-all-targets"), root, "[data-dev-reprint-target]");
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

function localDateTimeValue(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return new Date(date.getTime() - date.getTimezoneOffset() * 60000).toISOString().slice(0, 19);
}

function urlTimeValue(value) {
  return value ? new Date(value).toISOString() : "";
}

function filteredAPIParams(names, cursor = "") {
  const current = new URLSearchParams(window.location.search);
  const result = new URLSearchParams({ limit: "100" });
  names.forEach((name) => {
    const value = current.get(name);
    if (value) result.set(name, value);
  });
  if (cursor) result.set("cursor", cursor);
  return result;
}

function navigateWithFilters(path, params) {
  const query = params.toString();
  navigateDashboard(path + (query ? `?${query}` : ""));
}

const printerLogFilterNames = ["printerId", "event", "runUid", "q", "from", "to"];

function printerLogFiltersFromURL() {
  const params = new URLSearchParams(window.location.search);
  return Object.fromEntries(printerLogFilterNames.map((name) => [name, params.get(name) || ""]));
}

async function fetchPrinterLogEvents({ filters = {}, cursor = "", limit = 100 } = {}) {
  const params = new URLSearchParams({ limit: String(limit) });
  for (const name of printerLogFilterNames) {
    const value = filters[name];
    if (value) params.set(name, value);
  }
  if (cursor) params.set("cursor", cursor);
  return api(`/printer-logs?${params}`);
}

function printerLogRowsHTML(entries, { includePrinter = false, emptyMessage = "No matching printer events" } = {}) {
  const rows = entries.map((entry) => `<tr>
    <td class="muted log-time">${esc(detailDate(entry.createdAt))}</td><td>${esc(entry.type)}</td>
    ${includePrinter ? `<td>${esc(entry.printerId || "")}</td>` : ""}<td class="muted">${esc(entry.runUid || "")}</td>
    <td>${esc(entry.message || "")}</td></tr>`).join("");
  const columns = includePrinter ? 5 : 4;
  return rows || `<tr><td colspan="${columns}" class="muted">${esc(emptyMessage)}</td></tr>`;
}

function refreshPrinterLogPrinterOptions(selectedID) {
  const select = $("#printer-log-printer");
  if (!select) return;
  const selected = selectedID === undefined ? select.value : selectedID;
  const statuses = [...knownPrinters].sort((left, right) => {
    const leftName = left.printer.displayName || left.printer.id;
    const rightName = right.printer.displayName || right.printer.id;
    return leftName.localeCompare(rightName) || left.printer.id.localeCompare(right.printer.id);
  });
  const configuredIDs = new Set(statuses.map((status) => status.printer.id));
  const options = statuses.map((status) => {
    const printer = status.printer;
    const label = printer.displayName && printer.displayName !== printer.id
      ? `${printer.displayName} (${printer.id})`
      : printer.id;
    return `<option value="${esc(printer.id)}">${esc(label)}</option>`;
  });
  if (selected && !configuredIDs.has(selected)) {
    options.push(`<option value="${esc(selected)}">${esc(`Archived: ${selected}`)}</option>`);
  }
  select.innerHTML = `<option value="">All printers</option>${options.join("")}`;
  select.value = selected;
}

let systemLogRows = [];
let systemLogCursor = "";
let systemLogURL = "";
let systemLogExpanded = false;
let systemLogRetentionSeconds = 7200;

function syncSystemLogFilters() {
  const signature = window.location.pathname + window.location.search;
  if (systemLogURL === signature) return;
  systemLogURL = signature;
  const params = new URLSearchParams(window.location.search);
  const selected = new Set((params.get("levels") || "debug,info,warn,error").split(","));
  document.querySelectorAll('#system-log-filters input[name="level"]').forEach((input) => {
    input.checked = selected.has(input.value);
  });
  $("#system-log-q").value = params.get("q") || "";
  $("#system-log-printer").value = params.get("printerId") || "";
  $("#system-log-run").value = params.get("runUid") || "";
  $("#system-log-from").value = localDateTimeValue(params.get("from"));
  $("#system-log-to").value = localDateTimeValue(params.get("to"));
  systemLogRows = [];
  systemLogCursor = "";
  systemLogExpanded = false;
}

function renderSystemLogs() {
  $("#system-log-table tbody").innerHTML = systemLogRows.map((record) => {
    const attrs = record.attributes || {};
    const fields = Object.entries(attrs).slice(0, 6).map(([key, value]) =>
      `<span class="field-chip"><b>${esc(key)}</b>=${esc(typeof value === "string" ? value : JSON.stringify(value))}</span>`).join(" ");
    return `<tr><td class="muted log-time">${esc(new Date(record.createdAt).toLocaleString())}</td>
      <td><span class="badge log-${esc(record.level)}">${esc(record.level)}</span></td>
      <td>${esc(record.message)}</td><td class="log-fields">${fields}</td>
      <td><details class="raw-attributes"><summary>Raw</summary><pre>${esc(JSON.stringify(attrs, null, 2))}</pre></details></td></tr>`;
  }).join("") || `<tr><td colspan="5" class="muted">No matching system logs</td></tr>`;
  $("#btn-system-logs-older").classList.toggle("hidden", !systemLogCursor);
}

async function refreshSystemLogs({ append = false, automatic = false } = {}) {
  if ($("#tab-system-logs").classList.contains("hidden")) return;
  syncSystemLogFilters();
  try {
    const params = filteredAPIParams(["levels", "q", "printerId", "runUid", "from", "to"], append ? systemLogCursor : "");
    const result = await api(`/system-logs?${params}`);
    const incoming = result.logs || [];
    systemLogRetentionSeconds = result.retentionSeconds || systemLogRetentionSeconds;
    if (append) {
      systemLogRows.push(...incoming);
      systemLogExpanded = true;
    }
    else if (automatic) {
      if (!systemLogExpanded) {
        systemLogRows = incoming;
      } else {
        const known = new Set(incoming.map((record) => record.id));
        const cutoff = Date.now() - systemLogRetentionSeconds * 1000;
        systemLogRows = [...incoming, ...systemLogRows.filter((record) =>
          !known.has(record.id) && new Date(record.createdAt).getTime() >= cutoff)];
      }
    } else {
      systemLogRows = incoming;
      systemLogExpanded = false;
    }
    if (!automatic || !systemLogExpanded) systemLogCursor = result.nextCursor || "";
    renderSystemLogs();
  } catch (error) {
    if (!automatic) toast(error.message, true);
  }
}

$("#system-log-filters").addEventListener("submit", (event) => {
  event.preventDefault();
  const levels = [...document.querySelectorAll('#system-log-filters input[name="level"]:checked')].map((input) => input.value);
  if (!levels.length) return toast("Select at least one log level", true);
  const params = new URLSearchParams();
  if (levels.length !== 4) params.set("levels", levels.join(","));
  for (const [name, selector] of [["q", "#system-log-q"], ["printerId", "#system-log-printer"], ["runUid", "#system-log-run"]]) {
    const value = $(selector).value.trim();
    if (value) params.set(name, value);
  }
  for (const [name, selector] of [["from", "#system-log-from"], ["to", "#system-log-to"]]) {
    const value = urlTimeValue($(selector).value);
    if (value) params.set(name, value);
  }
  systemLogURL = "";
  navigateWithFilters("/admin/system/logs", params);
});
$("#btn-system-logs-clear").addEventListener("click", () => { systemLogURL = ""; navigateDashboard("/admin/system/logs"); });
$("#btn-system-logs-refresh").addEventListener("click", () => refreshSystemLogs());
$("#btn-system-logs-older").addEventListener("click", () => refreshSystemLogs({ append: true }));

let printerLogRows = [];
let printerLogCursor = "";
let printerLogURL = "";
let printerLogExpanded = false;
let printerLogRetentionSeconds = 172800;

function syncPrinterLogFilters() {
  const signature = window.location.pathname + window.location.search;
  if (printerLogURL === signature) return;
  printerLogURL = signature;
  const params = new URLSearchParams(window.location.search);
  refreshPrinterLogPrinterOptions(params.get("printerId") || "");
  $("#printer-log-event").value = params.get("event") || "";
  $("#printer-log-run").value = params.get("runUid") || "";
  $("#printer-log-q").value = params.get("q") || "";
  $("#printer-log-from").value = localDateTimeValue(params.get("from"));
  $("#printer-log-to").value = localDateTimeValue(params.get("to"));
  printerLogRows = [];
  printerLogCursor = "";
  printerLogExpanded = false;
}

function renderPrinterLogs() {
  $("#printer-log-table tbody").innerHTML = printerLogRowsHTML(printerLogRows, { includePrinter: true });
  $("#btn-printer-logs-older").classList.toggle("hidden", !printerLogCursor);
}

async function refreshPrinterLogs({ append = false, automatic = false } = {}) {
  if ($("#tab-printer-logs").classList.contains("hidden")) return;
  syncPrinterLogFilters();
  try {
    const result = await fetchPrinterLogEvents({
      filters: printerLogFiltersFromURL(),
      cursor: append ? printerLogCursor : "",
    });
    const incoming = result.events || [];
    printerLogRetentionSeconds = result.retentionSeconds || printerLogRetentionSeconds;
    if (append) {
      printerLogRows.push(...incoming);
      printerLogExpanded = true;
    }
    else if (automatic) {
      if (!printerLogExpanded) {
        printerLogRows = incoming;
      } else {
        const known = new Set(incoming.map((entry) => entry.id));
        const cutoff = Date.now() - printerLogRetentionSeconds * 1000;
        printerLogRows = [...incoming, ...printerLogRows.filter((entry) =>
          !known.has(entry.id) && new Date(entry.createdAt).getTime() >= cutoff)];
      }
    } else {
      printerLogRows = incoming;
      printerLogExpanded = false;
    }
    if (!automatic || !printerLogExpanded) printerLogCursor = result.nextCursor || "";
    renderPrinterLogs();
  } catch (error) {
    if (!automatic) toast(error.message, true);
  }
}

$("#printer-log-filters").addEventListener("submit", (event) => {
  event.preventDefault();
  const params = new URLSearchParams();
  for (const [name, selector] of [["printerId", "#printer-log-printer"], ["event", "#printer-log-event"],
    ["runUid", "#printer-log-run"], ["q", "#printer-log-q"]]) {
    const value = $(selector).value.trim();
    if (value) params.set(name, value);
  }
  for (const [name, selector] of [["from", "#printer-log-from"], ["to", "#printer-log-to"]]) {
    const value = urlTimeValue($(selector).value);
    if (value) params.set(name, value);
  }
  printerLogURL = "";
  navigateWithFilters("/admin/operations/printer-logs", params);
});
$("#btn-printer-logs-clear").addEventListener("click", () => { printerLogURL = ""; navigateDashboard("/admin/operations/printer-logs"); });
$("#btn-printer-logs-refresh").addEventListener("click", () => refreshPrinterLogs());
$("#btn-printer-logs-older").addEventListener("click", () => refreshPrinterLogs({ append: true }));

// ---- polling ----
refreshStatus();
initializeNavigation();
setInterval(refreshStatus, 2000);
setInterval(refreshQueue, 5000);
setInterval(() => {
  if (!$("#printer-detail-dialog").open) return;
  if (printerDetailTab === "queue") refreshPrinterDetailQueue();
  if (printerDetailTab === "logs") refreshPrinterDetailLogs();
}, 5000);
setInterval(refreshBluetoothDevices, 2000);
setInterval(() => refreshDevJob(true), 2000);
setInterval(() => refreshSystemLogs({ automatic: true }), 5000);
setInterval(() => refreshPrinterLogs({ automatic: true }), 5000);
