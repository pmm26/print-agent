"use strict";

const $ = (sel) => document.querySelector(sel);
const api = async (path, opts = {}) => {
  const res = await fetch("/api/v1" + path, {
    headers: { "Content-Type": "application/json" },
    ...opts,
  });
  const body = res.status === 204 ? null : await res.json().catch(() => null);
  if (!res.ok) throw new Error(body?.error || res.statusText);
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

// ---- tabs ----
document.querySelectorAll(".tab").forEach((btn) => {
  btn.addEventListener("click", () => {
    document.querySelectorAll(".tab").forEach((b) => b.classList.toggle("active", b === btn));
    document.querySelectorAll(".panel").forEach((p) => p.classList.add("hidden"));
    $("#tab-" + btn.dataset.tab).classList.remove("hidden");
    if (btn.dataset.tab === "queue") refreshQueue();
    if (btn.dataset.tab === "pairing") refreshPairing();
    if (btn.dataset.tab === "diagnostics") refreshDiagnostics();
    if (btn.dataset.tab === "logs") refreshLogs();
  });
});

const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) =>
  ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const fmtTime = (iso) => (iso ? new Date(iso).toLocaleTimeString() : "—");

// ---- printers ----
async function refreshStatus() {
  try {
    const data = await api("/status");
    $("#agent-state").textContent = "Running";
    $("#agent-state").className = "badge running";
    renderPrinters(data.printers || []);
  } catch {
    $("#agent-state").textContent = "Unreachable";
    $("#agent-state").className = "badge error";
  }
}

function renderPrinters(list) {
  const root = $("#printer-list");
  if (!list.length) {
    root.innerHTML = `<div class="card muted">No printers configured yet. Pair the printer in the OS Bluetooth settings, then use <b>Add printer</b>.</div>`;
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

function openDialog(printer) {
  editingID = printer?.id || null;
  $("#dialog-title").textContent = printer ? `Configure ${printer.id}` : "Add printer";
  $("#f-id").value = printer?.id || "";
  $("#f-id").disabled = !!printer;
  $("#f-name").value = printer?.displayName || "";
  $("#f-encoding").value = printer?.encoding || "CP858";
  $("#f-width").value = printer?.charactersPerLine || 32;
  loadCandidates(printer?.endpoint);
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
  const status = $("#queue-filter").value;
  const rows = (await api(`/deliveries?limit=100${status ? "&status=" + status : ""}`).catch(() => [])) || [];
  $("#queue-table tbody").innerHTML = rows.map((d) => {
    const actions = [];
    if (["failed", "uncertain", "transmitted"].includes(d.status))
      actions.push(`<button data-act="reprint">Reprint</button>`);
    if (["failed", "uncertain"].includes(d.status) && !d.resolvedAt)
      actions.push(`<button data-act="resolve">Mark resolved</button>`);
    if (["queued", "failed", "uncertain"].includes(d.status))
      actions.push(`<button data-act="cancel" class="danger">Cancel</button>`);
    return `<tr data-id="${esc(d.deliveryId)}">
      <td>${esc(d.deliveryId)}${d.reprintOf ? ' <span class="badge printing">reprint</span>' : ""}</td>
      <td>${esc(d.printerId)}</td><td>${esc(d.template)}</td>
      <td><span class="badge ${esc(d.status)}">${esc(d.status)}</span>${d.resolvedAt ? ' <span class="muted">resolved</span>' : ""}</td>
      <td>${d.attemptCount}</td>
      <td class="muted">${esc(d.lastError || "")}</td>
      <td class="row">${actions.join("")}</td>
    </tr>`;
  }).join("") || `<tr><td colspan="7" class="muted">No deliveries</td></tr>`;

  $("#queue-table").querySelectorAll("button[data-act]").forEach((btn) => {
    btn.addEventListener("click", () => {
      const id = encodeURIComponent(btn.closest("tr").dataset.id);
      const act = btn.dataset.act;
      if (act === "cancel" && !confirm("Cancel this delivery?")) return;
      call(() => api(`/deliveries/${id}/${act}`, { method: "POST" }), `${act} ok`);
    });
  });
}
$("#btn-queue-refresh").addEventListener("click", refreshQueue);
$("#queue-filter").addEventListener("change", refreshQueue);

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
      <td>${esc(e.printerId || "")}</td><td class="muted">${esc(e.deliveryId || "")}</td>
      <td>${esc(e.message || "")}</td>
    </tr>`).join("") || `<tr><td colspan="5" class="muted">No events</td></tr>`;
}
$("#btn-logs-refresh").addEventListener("click", refreshLogs);

// ---- polling ----
refreshStatus();
setInterval(refreshStatus, 2000);
setInterval(refreshQueue, 5000);
