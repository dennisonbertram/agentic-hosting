const STORAGE_KEY = "agentic-hosting-dashboard-session";

const state = {
  apiBase: "",
  apiKey: "",
  connected: false,
  loading: false,
  activeView: "overview",
  notice: null,
  refreshTimer: null,
  data: {
    health: null,
    services: [],
    builds: [],
    databases: [],
    tenant: null,
    tenantUsage: null,
    keys: [],
    activity: [],
  },
  selectedServiceId: null,
  selectedBuildId: null,
  selectedDatabaseId: null,
  selectedServiceEnv: null,
  selectedServiceEnvRevealed: false,
  selectedServiceLogs: "",
  selectedBuildLogs: "",
  selectedDatabaseConnectionString: "",
};

const elements = {};

document.addEventListener("DOMContentLoaded", () => {
  captureElements();
  bindEvents();
  restoreSession();
  render();
  if (state.apiKey) {
    void connectAndLoad(true);
  }
});

function captureElements() {
  elements.connectionForm = document.getElementById("connection-form");
  elements.apiBase = document.getElementById("api-base");
  elements.apiKey = document.getElementById("api-key");
  elements.disconnectButton = document.getElementById("disconnect-button");
  elements.navList = document.getElementById("nav-list");
  elements.pageTitle = document.getElementById("page-title");
  elements.connectionPill = document.getElementById("connection-pill");
  elements.refreshButton = document.getElementById("refresh-button");
  elements.notice = document.getElementById("notice");
  elements.sidebarHealth = document.getElementById("sidebar-health");
  elements.sidebarUsage = document.getElementById("sidebar-usage");
  elements.overviewKpis = document.getElementById("overview-kpis");
  elements.overviewHealth = document.getElementById("overview-health");
  elements.overviewIncidents = document.getElementById("overview-incidents");
  elements.overviewBuilds = document.getElementById("overview-builds");
  elements.overviewActivity = document.getElementById("overview-activity");
  elements.servicesTable = document.getElementById("services-table");
  elements.serviceDetail = document.getElementById("service-detail");
  elements.serviceCreateForm = document.getElementById("service-create-form");
  elements.buildCreateForm = document.getElementById("build-create-form");
  elements.buildServiceSelect = document.getElementById("build-service-select");
  elements.buildsTable = document.getElementById("builds-table");
  elements.buildDetail = document.getElementById("build-detail");
  elements.databaseCreateForm = document.getElementById("database-create-form");
  elements.databasesTable = document.getElementById("databases-table");
  elements.databaseDetail = document.getElementById("database-detail");
  elements.tenantSummary = document.getElementById("tenant-summary");
  elements.tenantUpdateForm = document.getElementById("tenant-update-form");
  elements.tenantSuspendButton = document.getElementById("tenant-suspend-button");
  elements.keyCreateForm = document.getElementById("key-create-form");
  elements.keyCreateResult = document.getElementById("key-create-result");
  elements.keysList = document.getElementById("keys-list");
  elements.activityList = document.getElementById("activity-list");
}

function bindEvents() {
  elements.connectionForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    await connectAndLoad(false);
  });

  elements.disconnectButton.addEventListener("click", () => {
    clearSession();
    render();
  });

  elements.refreshButton.addEventListener("click", async () => {
    await loadDashboard(false);
  });

  elements.navList.addEventListener("click", (event) => {
    const button = event.target.closest("[data-view]");
    if (!button) {
      return;
    }
    setActiveView(button.dataset.view);
  });

  elements.servicesTable.addEventListener("click", async (event) => {
    const row = event.target.closest("tr[data-id]");
    if (!row) {
      return;
    }
    await selectService(row.dataset.id);
  });

  elements.serviceDetail.addEventListener("click", async (event) => {
    const action = event.target.closest("[data-action]");
    if (!action) {
      return;
    }
    await handleServiceDetailAction(action.dataset.action, action.dataset.id || state.selectedServiceId);
  });

  elements.serviceDetail.addEventListener("submit", async (event) => {
    const form = event.target.closest("form");
    if (!form) {
      return;
    }
    event.preventDefault();
    if (form.dataset.role === "service-env-form") {
      await handleServiceEnvSave(form);
    }
  });

  elements.serviceCreateForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    await handleServiceCreate(event.target);
  });

  elements.buildCreateForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    await handleBuildCreate(event.target);
  });

  elements.buildsTable.addEventListener("click", async (event) => {
    const action = event.target.closest("[data-action]");
    const row = event.target.closest("tr[data-id]");
    if (action) {
      event.stopPropagation();
      await handleBuildAction(action.dataset.action, action.dataset.id);
      return;
    }
    if (row) {
      await selectBuild(row.dataset.id);
    }
  });

  elements.buildDetail.addEventListener("click", async (event) => {
    const action = event.target.closest("[data-action]");
    if (!action) {
      return;
    }
    await handleBuildAction(action.dataset.action, action.dataset.id || state.selectedBuildId);
  });

  elements.databaseCreateForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    await handleDatabaseCreate(event.target);
  });

  elements.databasesTable.addEventListener("click", async (event) => {
    const row = event.target.closest("tr[data-id]");
    if (!row) {
      return;
    }
    await selectDatabase(row.dataset.id);
  });

  elements.databaseDetail.addEventListener("click", async (event) => {
    const action = event.target.closest("[data-action]");
    if (!action) {
      return;
    }
    await handleDatabaseAction(action.dataset.action, action.dataset.id || state.selectedDatabaseId);
  });

  elements.tenantUpdateForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    await handleTenantUpdate(event.target);
  });

  elements.tenantSuspendButton.addEventListener("click", async () => {
    await suspendTenant();
  });

  elements.keyCreateForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    await handleKeyCreate(event.target);
  });

  elements.keysList.addEventListener("click", async (event) => {
    const action = event.target.closest("[data-action]");
    if (!action) {
      return;
    }
    await handleKeyAction(action.dataset.action, action.dataset.id);
  });
}

function restoreSession() {
  const saved = localStorage.getItem(STORAGE_KEY);
  const parsed = saved ? safeJsonParse(saved) : null;
  const defaultBase = window.location.origin;
  state.apiBase = normalizeBaseUrl(parsed?.apiBase || defaultBase);
  state.apiKey = parsed?.apiKey || "";
  state.activeView = sanitizeView(window.location.hash.slice(1) || parsed?.activeView || "overview");
  elements.apiBase.value = state.apiBase;
  elements.apiKey.value = state.apiKey;
}

function saveSession() {
  localStorage.setItem(STORAGE_KEY, JSON.stringify({
    apiBase: state.apiBase,
    apiKey: state.apiKey,
    activeView: state.activeView,
  }));
}

function clearSession() {
  localStorage.removeItem(STORAGE_KEY);
  state.apiKey = "";
  state.connected = false;
  state.notice = null;
  state.data = {
    health: null,
    services: [],
    builds: [],
    databases: [],
    tenant: null,
    tenantUsage: null,
    keys: [],
    activity: [],
  };
  state.selectedServiceId = null;
  state.selectedBuildId = null;
  state.selectedDatabaseId = null;
  state.selectedServiceEnv = null;
  state.selectedServiceLogs = "";
  state.selectedBuildLogs = "";
  state.selectedDatabaseConnectionString = "";
  elements.apiKey.value = "";
  stopRefreshTimer();
}

async function connectAndLoad(silent) {
  state.apiBase = normalizeBaseUrl(elements.apiBase.value || window.location.origin);
  state.apiKey = elements.apiKey.value.trim();
  if (!state.apiKey) {
    showNotice("Enter an API key first.", "error");
    render();
    return;
  }
  saveSession();
  await loadDashboard(silent);
}

async function loadDashboard(silent) {
  if (!state.apiKey || state.loading) {
    return;
  }
  state.loading = true;
  if (!silent) {
    showNotice("Refreshing dashboard data...", "info");
    render();
  }

  try {
    const [
      health,
      services,
      builds,
      databases,
      tenant,
      tenantUsage,
      keys,
      activity,
    ] = await Promise.all([
      apiRequest("/v1/system/health/detailed"),
      apiRequest("/v1/services"),
      apiRequest("/v1/builds?limit=100"),
      apiRequest("/v1/databases"),
      apiRequest("/v1/tenant"),
      apiRequest("/v1/tenant/usage"),
      apiRequest("/v1/auth/keys"),
      apiRequest("/v1/activity?limit=100"),
    ]);

    state.data = { health, services, builds, databases, tenant, tenantUsage, keys, activity };
    state.connected = true;

    if (!services.some((service) => service.id === state.selectedServiceId)) {
      state.selectedServiceId = services[0]?.id || null;
      state.selectedServiceEnv = null;
      state.selectedServiceLogs = "";
    }
    if (!builds.some((build) => build.id === state.selectedBuildId)) {
      state.selectedBuildId = builds[0]?.id || null;
      state.selectedBuildLogs = "";
    }
    if (!databases.some((database) => database.id === state.selectedDatabaseId)) {
      state.selectedDatabaseId = databases[0]?.id || null;
      state.selectedDatabaseConnectionString = "";
    }

    if (state.selectedServiceId) {
      await loadSelectedServiceDetails(false);
    }
    if (state.selectedBuildId) {
      await loadSelectedBuildLogs();
    }

    state.notice = silent ? null : { type: "info", message: "Dashboard refreshed." };
    startRefreshTimer();
  } catch (error) {
    state.connected = false;
    state.notice = { type: "error", message: error.message };
  } finally {
    state.loading = false;
    render();
  }
}

function startRefreshTimer() {
  stopRefreshTimer();
  state.refreshTimer = window.setInterval(() => {
    void loadDashboard(true);
  }, 20000);
}

function stopRefreshTimer() {
  if (state.refreshTimer) {
    window.clearInterval(state.refreshTimer);
    state.refreshTimer = null;
  }
}

function render() {
  renderNav();
  renderHeader();
  renderNotice();
  renderSidebar();
  renderOverview();
  renderServices();
  renderBuilds();
  renderDatabases();
  renderTenant();
  renderActivity();
}

function renderNav() {
  const labels = {
    overview: "Overview",
    services: "Services",
    builds: "Builds",
    databases: "Databases",
    tenant: "Tenant",
    activity: "Activity",
  };
  elements.pageTitle.textContent = labels[state.activeView];
  document.querySelectorAll(".view").forEach((view) => {
    view.classList.toggle("active", view.id === `view-${state.activeView}`);
  });
  document.querySelectorAll(".nav-item").forEach((button) => {
    button.classList.toggle("active", button.dataset.view === state.activeView);
  });
}

function renderHeader() {
  elements.connectionPill.className = `connection-pill ${state.connected ? "connected" : "disconnected"}`;
  elements.connectionPill.textContent = state.connected
    ? `${trimBaseUrl(state.apiBase)} · connected`
    : "Disconnected";
}

function renderNotice() {
  if (!state.notice) {
    elements.notice.hidden = true;
    elements.notice.textContent = "";
    elements.notice.className = "notice";
    return;
  }
  elements.notice.hidden = false;
  elements.notice.textContent = state.notice.message;
  elements.notice.className = `notice${state.notice.type === "error" ? " error" : ""}`;
}

function renderSidebar() {
  if (!state.connected) {
    elements.sidebarHealth.innerHTML = `<div class="empty-state">Connect to load tenant health.</div>`;
    elements.sidebarUsage.innerHTML = `<div class="empty-state">Quota and usage appear here after auth.</div>`;
    return;
  }

  const health = state.data.health || {};
  const disk = health.disk || {};
  const healthItems = [
    { label: "Platform", value: health.status || "unknown", tone: toneForHealth(health.status) },
    { label: "Docker", value: health.docker?.available ? health.docker.version : "missing", tone: health.docker?.available ? "good" : "bad" },
    { label: "gVisor", value: health.gvisor?.available ? health.gvisor.version : "missing", tone: health.gvisor?.available ? "good" : "bad" },
    { label: "Disk", value: Number.isFinite(disk.used_percent) ? `${disk.used_percent}% used` : "unknown", tone: toneForDisk(disk.used_percent) },
  ];
  elements.sidebarHealth.innerHTML = healthItems.map((item) => `
    <div class="sidebar-kv">
      <div>
        <strong>${escapeHtml(item.label)}</strong>
        <span>${escapeHtml(item.value)}</span>
      </div>
      ${statusBadge(item.tone, item.tone)}
    </div>
  `).join("");

  const usage = state.data.tenantUsage;
  if (!usage) {
    elements.sidebarUsage.innerHTML = `<div class="empty-state">Usage unavailable.</div>`;
    return;
  }
  const buckets = [
    { label: "Services", used: usage.services.used, max: usage.services.max },
    { label: "Databases", used: usage.databases.used, max: usage.databases.max },
    { label: "API keys", used: usage.api_keys.used, max: usage.api_keys.max },
  ];
  elements.sidebarUsage.innerHTML = buckets.map((bucket) => {
    const percent = bucket.max > 0 ? Math.min(100, Math.round((bucket.used / bucket.max) * 100)) : 0;
    return `
      <div class="sidebar-kv">
        <div style="width:100%">
          <strong>${escapeHtml(bucket.label)}</strong>
          <span>${bucket.used} / ${bucket.max}</span>
          <div class="usage-meter"><span style="width:${percent}%"></span></div>
        </div>
      </div>
    `;
  }).join("");
}

function renderOverview() {
  if (!state.connected) {
    elements.overviewKpis.innerHTML = emptyCard("Connect to load overview metrics.");
    elements.overviewHealth.innerHTML = emptyCard("Health data will render here.");
    elements.overviewIncidents.innerHTML = `<div class="empty-state">No tenant data yet.</div>`;
    elements.overviewBuilds.innerHTML = `<div class="empty-state">Builds appear here.</div>`;
    elements.overviewActivity.innerHTML = `<div class="empty-state">Recent activity appears here.</div>`;
    return;
  }

  const { services, builds, databases, health, tenantUsage, activity } = state.data;
  const incidents = deriveIncidents();

  const kpis = [
    { label: "Services", value: services.length, meta: countStatuses(services.map((service) => service.status)) },
    { label: "Builds", value: builds.length, meta: countStatuses(builds.map((build) => build.status)) },
    { label: "Databases", value: databases.length, meta: countStatuses(databases.map((database) => database.status)) },
    { label: "Incidents", value: incidents.length, meta: health?.status === "ok" ? "Host healthy" : `Host ${health?.status || "unknown"}` },
  ];
  elements.overviewKpis.innerHTML = kpis.map((kpi) => `
    <article class="kpi-card">
      <div class="kpi-label">${escapeHtml(kpi.label)}</div>
      <div class="kpi-value">${escapeHtml(String(kpi.value))}</div>
      <div class="kpi-meta">${escapeHtml(kpi.meta)}</div>
    </article>
  `).join("");

  elements.overviewHealth.innerHTML = [
    healthCard("Platform", health?.status || "unknown", toneForHealth(health?.status), "Overall control-plane health"),
    healthCard("Docker", health?.docker?.available ? health.docker.version : "Unavailable", health?.docker?.available ? "good" : "bad", "Container runtime availability"),
    healthCard("gVisor", health?.gvisor?.available ? health.gvisor.version : "Unavailable", health?.gvisor?.available ? "good" : "bad", "Sandbox runtime availability"),
    healthCard("Disk", Number.isFinite(health?.disk?.used_percent) ? `${health.disk.used_percent}% used` : "Unknown", toneForDisk(health?.disk?.used_percent), tenantUsage ? `${tenantUsage.disk_gb} GB tenant quota` : "Disk telemetry"),
  ].join("");

  elements.overviewIncidents.innerHTML = incidents.length
    ? incidents.slice(0, 8).map(renderIncident).join("")
    : `<div class="empty-state">No active incidents detected from current platform state.</div>`;

  elements.overviewBuilds.innerHTML = state.data.builds.length
    ? state.data.builds.slice(0, 6).map((build) => `
      <div class="list-item">
        <div class="list-item-head">
          <div class="list-item-title">${escapeHtml(build.service_name || build.service_id)}</div>
          ${statusBadge(toneForStatus(build.status), build.status)}
        </div>
        <div class="list-item-subtitle mono">${escapeHtml(build.source_ref || "main")} · ${escapeHtml(shortId(build.id))}</div>
      </div>
    `).join("")
    : `<div class="empty-state">No builds yet.</div>`;

  elements.overviewActivity.innerHTML = activity.length
    ? activity.slice(0, 8).map(renderActivityItem).join("")
    : `<div class="empty-state">No recent activity yet.</div>`;
}

function renderServices() {
  const services = state.data.services || [];
  elements.buildServiceSelect.innerHTML = services.length
    ? services.map((service) => `<option value="${escapeHtml(service.id)}">${escapeHtml(service.name)}</option>`).join("")
    : `<option value="">No services</option>`;

  elements.servicesTable.innerHTML = services.length
    ? services.map((service) => `
      <tr data-id="${escapeHtml(service.id)}" class="${service.id === state.selectedServiceId ? "selected" : ""}">
        <td>
          <div><strong>${escapeHtml(service.name)}</strong></div>
          <div class="muted mono">${escapeHtml(shortId(service.id))}</div>
        </td>
        <td>${statusBadge(toneForService(service), service.circuit_open ? "circuit open" : service.status)}</td>
        <td class="mono">${service.url ? escapeHtml(service.url) : "n/a"}</td>
        <td class="mono">${escapeHtml(service.image || "")}</td>
        <td>${escapeHtml(String(service.crash_count || 0))}</td>
        <td>${escapeHtml(formatTimestamp(service.updated_at))}</td>
      </tr>
    `).join("")
    : `<tr><td colspan="6"><div class="empty-state">No services yet.</div></td></tr>`;

  const service = services.find((item) => item.id === state.selectedServiceId);
  if (!service) {
    elements.serviceDetail.innerHTML = `<div class="empty-state">Select a service.</div>`;
    return;
  }

  const serviceBuilds = state.data.builds.filter((build) => build.service_id === service.id).slice(0, 5);
  const envEntries = state.selectedServiceEnv ? Object.entries(state.selectedServiceEnv) : [];
  elements.serviceDetail.innerHTML = `
    <div class="detail-group">
      <div class="detail-label">Identity</div>
      <div class="detail-row"><strong>${escapeHtml(service.name)}</strong>${statusBadge(toneForService(service), service.circuit_open ? "circuit open" : service.status)}</div>
      <div class="detail-row"><span>Service ID</span><span class="mono">${escapeHtml(service.id)}</span></div>
      <div class="detail-row"><span>Public URL</span><span class="mono">${service.url ? escapeHtml(service.url) : "n/a"}</span></div>
      <div class="detail-row"><span>Image</span><span class="mono">${escapeHtml(service.image || "")}</span></div>
      <div class="detail-row"><span>Last error</span><span>${escapeHtml(service.last_error || "None")}</span></div>
      <div class="detail-row"><span>Crash count</span><span>${escapeHtml(String(service.crash_count || 0))}</span></div>
    </div>

    <div class="detail-group">
      <div class="detail-label">Controls</div>
      <div class="control-row">
        <button class="tiny-button" data-action="service-start" data-id="${escapeHtml(service.id)}" type="button">Start</button>
        <button class="tiny-button" data-action="service-stop" data-id="${escapeHtml(service.id)}" type="button">Stop</button>
        <button class="tiny-button" data-action="service-restart" data-id="${escapeHtml(service.id)}" type="button">Restart</button>
        <button class="ghost-button" data-action="service-reset" data-id="${escapeHtml(service.id)}" type="button">Reset circuit</button>
        <button class="ghost-button" data-action="service-refresh-logs" data-id="${escapeHtml(service.id)}" type="button">Refresh logs</button>
        <button class="danger-button" data-action="service-delete" data-id="${escapeHtml(service.id)}" type="button">Delete</button>
      </div>
    </div>

    <div class="detail-group">
      <div class="detail-label">Environment</div>
      <div class="inline-actions">
        <button class="tiny-button" data-action="service-env-mask" type="button">Masked</button>
        <button class="tiny-button" data-action="service-env-reveal" type="button">Reveal</button>
      </div>
      ${envEntries.length ? envEntries.map(([key, value]) => `
        <div class="detail-row">
          <div>
            <strong class="mono">${escapeHtml(key)}</strong>
            <div class="detail-copy mono">${escapeHtml(value)}</div>
          </div>
          <button class="danger-button" data-action="service-env-delete" data-id="${escapeHtml(key)}" type="button">Delete</button>
        </div>
      `).join("") : `<div class="empty-state">No env vars loaded for this service.</div>`}
      <form data-role="service-env-form" class="form-grid" style="margin-top:12px">
        <label class="field">
          <span>Key</span>
          <input name="key" required placeholder="API_URL">
        </label>
        <label class="field">
          <span>Value</span>
          <input name="value" required placeholder="https://example.com">
        </label>
        <div class="form-actions field-wide">
          <button class="primary-button" type="submit">Save env var</button>
        </div>
      </form>
    </div>

    <div class="detail-group">
      <div class="detail-label">Recent builds</div>
      ${serviceBuilds.length ? serviceBuilds.map((build) => `
        <div class="detail-row">
          <div>
            <strong>${escapeHtml(build.source_ref || "main")}</strong>
            <div class="detail-copy mono">${escapeHtml(shortId(build.id))}</div>
          </div>
          ${statusBadge(toneForStatus(build.status), build.status)}
        </div>
      `).join("") : `<div class="empty-state">No builds for this service yet.</div>`}
    </div>

    <div class="detail-group">
      <div class="detail-label">Logs</div>
      <div class="log-block">${escapeHtml(state.selectedServiceLogs || "No logs loaded.")}</div>
    </div>
  `;
}

function renderBuilds() {
  const builds = state.data.builds || [];
  elements.buildsTable.innerHTML = builds.length
    ? builds.map((build) => `
      <tr data-id="${escapeHtml(build.id)}" class="${build.id === state.selectedBuildId ? "selected" : ""}">
        <td>
          <div><strong>${escapeHtml(build.service_name || build.service_id)}</strong></div>
          <div class="muted mono">${escapeHtml(shortId(build.id))}</div>
        </td>
        <td>${statusBadge(toneForStatus(build.status), build.status)}</td>
        <td class="mono">${escapeHtml(build.source_ref || "main")}</td>
        <td class="mono">${escapeHtml(truncate(build.image || "", 28))}</td>
        <td>${escapeHtml(formatTimestamp(build.created_at))}</td>
      </tr>
    `).join("")
    : `<tr><td colspan="5"><div class="empty-state">No builds yet.</div></td></tr>`;

  const build = builds.find((item) => item.id === state.selectedBuildId);
  if (!build) {
    elements.buildDetail.innerHTML = `<div class="empty-state">Select a build.</div>`;
    return;
  }

  elements.buildDetail.innerHTML = `
    <div class="detail-group">
      <div class="detail-label">Identity</div>
      <div class="detail-row"><strong>${escapeHtml(build.service_name || build.service_id)}</strong>${statusBadge(toneForStatus(build.status), build.status)}</div>
      <div class="detail-row"><span>Build ID</span><span class="mono">${escapeHtml(build.id)}</span></div>
      <div class="detail-row"><span>Source</span><span class="mono">${escapeHtml(build.source_url || "")}</span></div>
      <div class="detail-row"><span>Ref</span><span class="mono">${escapeHtml(build.source_ref || "main")}</span></div>
      <div class="detail-row"><span>Image</span><span class="mono">${escapeHtml(build.image || "")}</span></div>
      <div class="detail-row"><span>Created</span><span>${escapeHtml(formatTimestamp(build.created_at))}</span></div>
      <div class="detail-row"><span>Started</span><span>${escapeHtml(formatTimestamp(build.started_at))}</span></div>
      <div class="detail-row"><span>Finished</span><span>${escapeHtml(formatTimestamp(build.finished_at))}</span></div>
    </div>
    <div class="detail-group">
      <div class="detail-label">Actions</div>
      <div class="control-row">
        <button class="tiny-button" data-action="build-refresh" data-id="${escapeHtml(build.id)}" type="button">Refresh logs</button>
        <button class="danger-button" data-action="build-cancel" data-id="${escapeHtml(build.id)}" type="button">Cancel build</button>
      </div>
    </div>
    <div class="detail-group">
      <div class="detail-label">Logs</div>
      <div class="log-block">${escapeHtml(state.selectedBuildLogs || "No logs loaded.")}</div>
    </div>
  `;
}

function renderDatabases() {
  const databases = state.data.databases || [];
  elements.databasesTable.innerHTML = databases.length
    ? databases.map((database) => `
      <tr data-id="${escapeHtml(database.id)}" class="${database.id === state.selectedDatabaseId ? "selected" : ""}">
        <td>
          <div><strong>${escapeHtml(database.name)}</strong></div>
          <div class="muted mono">${escapeHtml(shortId(database.id))}</div>
        </td>
        <td>${escapeHtml(database.type)}</td>
        <td>${statusBadge(toneForStatus(database.status), database.status)}</td>
        <td class="mono">${escapeHtml(database.host || "127.0.0.1")}</td>
        <td>${escapeHtml(String(database.port || ""))}</td>
        <td>${escapeHtml(formatTimestamp(database.created_at))}</td>
      </tr>
    `).join("")
    : `<tr><td colspan="6"><div class="empty-state">No databases yet.</div></td></tr>`;

  const database = databases.find((item) => item.id === state.selectedDatabaseId);
  if (!database) {
    elements.databaseDetail.innerHTML = `<div class="empty-state">Select a database.</div>`;
    return;
  }

  elements.databaseDetail.innerHTML = `
    <div class="detail-group">
      <div class="detail-label">Identity</div>
      <div class="detail-row"><strong>${escapeHtml(database.name)}</strong>${statusBadge(toneForStatus(database.status), database.status)}</div>
      <div class="detail-row"><span>Type</span><span class="mono">${escapeHtml(database.type)}</span></div>
      <div class="detail-row"><span>Database ID</span><span class="mono">${escapeHtml(database.id)}</span></div>
      <div class="detail-row"><span>Host</span><span class="mono">${escapeHtml(database.host || "127.0.0.1")}</span></div>
      <div class="detail-row"><span>Port</span><span>${escapeHtml(String(database.port || ""))}</span></div>
      <div class="detail-row"><span>Username</span><span class="mono">${escapeHtml(database.username || "n/a")}</span></div>
    </div>
    <div class="detail-group">
      <div class="detail-label">Actions</div>
      <div class="control-row">
        <button class="tiny-button" data-action="database-reveal" data-id="${escapeHtml(database.id)}" type="button">Reveal connection string</button>
        <button class="tiny-button" data-action="database-copy" data-id="${escapeHtml(database.id)}" type="button">Copy connection string</button>
        <button class="danger-button" data-action="database-delete" data-id="${escapeHtml(database.id)}" type="button">Delete</button>
      </div>
      <div class="code-block">${escapeHtml(state.selectedDatabaseConnectionString || "Connection string hidden.")}</div>
    </div>
  `;
}

function renderTenant() {
  const { tenant, tenantUsage, keys } = state.data;
  if (!tenant || !tenantUsage) {
    elements.tenantSummary.innerHTML = `<div class="empty-state">Connect to load tenant details.</div>`;
    elements.keysList.innerHTML = `<div class="empty-state">No keys loaded.</div>`;
    elements.keyCreateResult.hidden = true;
    return;
  }

  const usageRows = [
    renderUsageRow("Services", tenantUsage.services.used, tenantUsage.services.max),
    renderUsageRow("Databases", tenantUsage.databases.used, tenantUsage.databases.max),
    renderUsageRow("API keys", tenantUsage.api_keys.used, tenantUsage.api_keys.max),
    renderStaticRow("Memory quota", `${tenantUsage.memory_mb} MB`),
    renderStaticRow("CPU quota", `${tenantUsage.cpu_cores} cores`),
    renderStaticRow("Disk quota", `${tenantUsage.disk_gb} GB`),
    renderStaticRow("Rate limit", `${tenantUsage.rate_limit} req/s`),
  ].join("");

  elements.tenantSummary.innerHTML = `
    <div class="detail-group">
      <div class="detail-label">Identity</div>
      <div class="detail-row"><strong>${escapeHtml(tenant.name)}</strong>${statusBadge(toneForStatus(tenant.status), tenant.status)}</div>
      <div class="detail-row"><span>Email</span><span>${escapeHtml(tenant.email)}</span></div>
      <div class="detail-row"><span>Tenant ID</span><span class="mono">${escapeHtml(tenant.id)}</span></div>
      <div class="detail-row"><span>Created</span><span>${escapeHtml(formatTimestamp(tenant.created_at))}</span></div>
    </div>
    <div class="detail-group">
      <div class="detail-label">Quotas and usage</div>
      ${usageRows}
    </div>
  `;

  const tenantNameInput = elements.tenantUpdateForm.elements.namedItem("name");
  if (tenantNameInput && document.activeElement !== tenantNameInput) {
    tenantNameInput.value = tenant.name;
  }

  elements.keysList.innerHTML = keys.length
    ? keys.map((key) => `
      <div class="list-item">
        <div class="list-item-head">
          <div>
            <div class="list-item-title">${escapeHtml(key.name)}</div>
            <div class="list-item-subtitle mono">${escapeHtml(key.id)} · ${escapeHtml(key.prefix)}</div>
          </div>
          <button class="danger-button" data-action="key-revoke" data-id="${escapeHtml(key.id)}" type="button">Revoke</button>
        </div>
        <div class="list-item-subtitle">Created ${escapeHtml(formatTimestamp(key.created_at))}${key.expires_at ? ` · expires ${escapeHtml(formatTimestamp(key.expires_at))}` : ""}</div>
      </div>
    `).join("")
    : `<div class="empty-state">No active API keys.</div>`;
}

function renderActivity() {
  const activity = state.data.activity || [];
  elements.activityList.innerHTML = activity.length
    ? activity.map(renderActivityItem).join("")
    : `<div class="empty-state">No recent activity.</div>`;
}

async function selectService(serviceId) {
  state.selectedServiceId = serviceId;
  state.selectedServiceEnv = null;
  state.selectedServiceEnvRevealed = false;
  state.selectedServiceLogs = "";
  render();
  await loadSelectedServiceDetails(false);
  render();
}

async function loadSelectedServiceDetails(revealEnv) {
  const service = state.data.services.find((item) => item.id === state.selectedServiceId);
  if (!service) {
    return;
  }
  const [envResult, logsResult] = await Promise.allSettled([
    apiRequest(`/v1/services/${service.id}/env${revealEnv ? "?reveal=true" : ""}`),
    apiRequest(`/v1/services/${service.id}/logs?tail=200`, { responseType: "text" }),
  ]);
  state.selectedServiceEnv = envResult.status === "fulfilled" ? envResult.value : {};
  state.selectedServiceEnvRevealed = revealEnv;
  state.selectedServiceLogs = logsResult.status === "fulfilled" ? (logsResult.value || "") : `Log stream unavailable: ${logsResult.reason.message}`;
}

async function handleServiceDetailAction(action, id) {
  const serviceId = state.selectedServiceId;
  const resourceId = id;
  if (!serviceId) {
    return;
  }

  switch (action) {
    case "service-start":
      await runServiceAction(serviceId, "start");
      break;
    case "service-stop":
      await runServiceAction(serviceId, "stop");
      break;
    case "service-restart":
      await runServiceAction(serviceId, "restart");
      break;
    case "service-reset":
      await runServiceAction(serviceId, "reset");
      break;
    case "service-delete":
      if (!window.confirm("Delete this service? This removes the container record and service entry.")) {
        return;
      }
      await apiRequest(`/v1/services/${serviceId}`, { method: "DELETE" });
      if (state.selectedServiceId === serviceId) {
        state.selectedServiceId = null;
        state.selectedServiceEnv = null;
        state.selectedServiceLogs = "";
      }
      showNotice("Service deleted.", "info");
      await loadDashboard(true);
      break;
    case "service-refresh-logs":
      await refreshServiceLogs();
      break;
    case "service-env-reveal":
      await loadSelectedServiceDetails(true);
      showNotice("Environment variables revealed for the selected service.", "info");
      render();
      break;
    case "service-env-mask":
      await loadSelectedServiceDetails(false);
      render();
      break;
    case "service-env-delete":
      if (!window.confirm(`Delete env var ${resourceId}?`)) {
        return;
      }
      await apiRequest(`/v1/services/${state.selectedServiceId}/env/${encodeURIComponent(resourceId)}`, { method: "DELETE" });
      showNotice(`Deleted env var ${resourceId}.`, "info");
      await loadSelectedServiceDetails(state.selectedServiceEnvRevealed);
      await loadDashboard(true);
      break;
    default:
      break;
  }
}

async function runServiceAction(serviceId, action) {
  await apiRequest(`/v1/services/${serviceId}/${action}`, { method: "POST" });
  showNotice(`Service ${action} requested.`, "info");
  await loadDashboard(true);
}

async function refreshServiceLogs() {
  if (!state.selectedServiceId) {
    return;
  }
  try {
    state.selectedServiceLogs = await apiRequest(`/v1/services/${state.selectedServiceId}/logs?tail=200`, { responseType: "text" });
  } catch (error) {
    state.selectedServiceLogs = `Log stream unavailable: ${error.message}`;
  }
  showNotice("Service logs refreshed.", "info");
  render();
}

async function handleServiceEnvSave(form) {
  const key = form.elements.namedItem("key").value.trim();
  const value = form.elements.namedItem("value").value;
  if (!key) {
    showNotice("Env key is required.", "error");
    render();
    return;
  }
  await apiRequest(`/v1/services/${state.selectedServiceId}/env`, {
    method: "POST",
    body: { [key]: value },
  });
  form.reset();
  showNotice(`Saved env var ${key}.`, "info");
  await loadSelectedServiceDetails(state.selectedServiceEnvRevealed);
  await loadDashboard(true);
}

async function handleServiceCreate(form) {
  const envText = form.elements.namedItem("env").value.trim();
  let env = {};
  if (envText) {
    env = safeJsonParse(envText);
    if (!env || typeof env !== "object" || Array.isArray(env)) {
      showNotice("Env JSON must be an object like {\"KEY\":\"value\"}.", "error");
      render();
      return;
    }
  }
  await apiRequest("/v1/services", {
    method: "POST",
    body: {
      name: form.elements.namedItem("name").value.trim(),
      image: form.elements.namedItem("image").value.trim(),
      port: Number(form.elements.namedItem("port").value) || 80,
      env,
    },
  });
  form.reset();
  form.elements.namedItem("port").value = 80;
  showNotice("Service created. Deployment has started asynchronously.", "info");
  await loadDashboard(true);
}

async function handleBuildCreate(form) {
  const serviceId = form.elements.namedItem("serviceId").value;
  if (!serviceId) {
    showNotice("Create a service first.", "error");
    render();
    return;
  }
  await apiRequest(`/v1/services/${serviceId}/builds`, {
    method: "POST",
    body: {
      source_type: "git",
      source_url: form.elements.namedItem("sourceUrl").value.trim(),
      source_ref: form.elements.namedItem("sourceRef").value.trim() || "main",
    },
  });
  form.reset();
  form.elements.namedItem("sourceRef").value = "main";
  showNotice("Build queued.", "info");
  await loadDashboard(true);
}

async function selectBuild(buildId) {
  state.selectedBuildId = buildId;
  state.selectedBuildLogs = "";
  render();
  await loadSelectedBuildLogs();
  render();
}

async function loadSelectedBuildLogs() {
  const build = state.data.builds.find((item) => item.id === state.selectedBuildId);
  if (!build) {
    return;
  }
  try {
    state.selectedBuildLogs = await apiRequest(`/v1/services/${build.service_id}/builds/${build.id}/logs`, { responseType: "text" });
  } catch (error) {
    state.selectedBuildLogs = `Build logs unavailable: ${error.message}`;
  }
}

async function handleBuildAction(action, buildId) {
  const build = state.data.builds.find((item) => item.id === buildId);
  if (!build) {
    return;
  }
  if (action === "build-cancel") {
    if (!window.confirm("Cancel this build?")) {
      return;
    }
    await apiRequest(`/v1/services/${build.service_id}/builds/${build.id}`, { method: "DELETE" });
    showNotice("Build cancelled.", "info");
    await loadDashboard(true);
    return;
  }
  if (action === "build-refresh") {
    await loadSelectedBuildLogs();
    showNotice("Build logs refreshed.", "info");
    render();
  }
}

async function handleDatabaseCreate(form) {
  await apiRequest("/v1/databases", {
    method: "POST",
    body: {
      name: form.elements.namedItem("name").value.trim(),
      type: form.elements.namedItem("type").value,
    },
  });
  form.reset();
  showNotice("Database provisioning started.", "info");
  await loadDashboard(true);
}

async function selectDatabase(databaseId) {
  state.selectedDatabaseId = databaseId;
  state.selectedDatabaseConnectionString = "";
  render();
}

async function handleDatabaseAction(action, databaseId) {
  if (!databaseId) {
    return;
  }
  if (action === "database-reveal") {
    const response = await apiRequest(`/v1/databases/${databaseId}/connection-string`);
    state.selectedDatabaseConnectionString = response.connection_string || "";
    showNotice("Connection string revealed.", "info");
    render();
    return;
  }
  if (action === "database-copy") {
    if (!state.selectedDatabaseConnectionString) {
      showNotice("Reveal the connection string first.", "error");
      render();
      return;
    }
    await navigator.clipboard.writeText(state.selectedDatabaseConnectionString);
    showNotice("Connection string copied.", "info");
    render();
    return;
  }
  if (action === "database-delete") {
    if (!window.confirm("Delete this database? Its container and volume will be removed.")) {
      return;
    }
    await apiRequest(`/v1/databases/${databaseId}`, { method: "DELETE" });
    if (state.selectedDatabaseId === databaseId) {
      state.selectedDatabaseId = null;
      state.selectedDatabaseConnectionString = "";
    }
    showNotice("Database deleted.", "info");
    await loadDashboard(true);
  }
}

async function handleTenantUpdate(form) {
  const name = form.elements.namedItem("name").value.trim();
  await apiRequest("/v1/tenant", {
    method: "PATCH",
    body: { name },
  });
  showNotice("Tenant name updated.", "info");
  await loadDashboard(true);
}

async function suspendTenant() {
  if (!window.confirm("Suspend this tenant? All services stop and current keys are revoked.")) {
    return;
  }
  await apiRequest("/v1/tenant", { method: "DELETE" });
  showNotice("Tenant suspended. Current credentials are no longer valid.", "info");
  clearSession();
  render();
}

async function handleKeyCreate(form) {
  const expiresRaw = form.elements.namedItem("expiresIn").value.trim();
  const body = {
    name: form.elements.namedItem("name").value.trim(),
  };
  if (expiresRaw) {
    body.expires_in = Number(expiresRaw);
  }
  const response = await apiRequest("/v1/auth/keys", {
    method: "POST",
    body,
  });
  form.reset();
  elements.keyCreateResult.hidden = false;
  elements.keyCreateResult.textContent = response.api_key || "No key returned.";
  showNotice("API key created. Save it now; it will not be returned again.", "info");
  await loadDashboard(true);
}

async function handleKeyAction(action, keyId) {
  if (action !== "key-revoke" || !keyId) {
    return;
  }
  if (!window.confirm("Revoke this API key?")) {
    return;
  }
  await apiRequest(`/v1/auth/keys/${keyId}`, { method: "DELETE" });
  showNotice("API key revoked.", "info");
  await loadDashboard(true);
}

async function apiRequest(path, options = {}) {
  const {
    method = "GET",
    body,
    responseType = "json",
  } = options;

  const headers = {
    Authorization: `Bearer ${state.apiKey}`,
  };
  let payload;
  if (body !== undefined) {
    headers["Content-Type"] = "application/json";
    payload = JSON.stringify(body);
  }

  const response = await fetch(`${state.apiBase}${path}`, {
    method,
    headers,
    body: payload,
  });

  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    const contentType = response.headers.get("content-type") || "";
    if (contentType.includes("application/json")) {
      const errorPayload = await response.json();
      message = errorPayload.error || errorPayload.message || message;
    } else {
      const text = await response.text();
      if (text) {
        message = text;
      }
    }
    throw new Error(message);
  }

  if (responseType === "text") {
    return response.text();
  }
  if (response.status === 204) {
    return null;
  }
  return response.json();
}

function deriveIncidents() {
  const incidents = [];
  const health = state.data.health;
  if (health && health.status && health.status !== "ok") {
    incidents.push({
      title: "Platform health degraded",
      description: `Detailed health endpoint reports ${health.status}.`,
      tone: "bad",
    });
  }
  if (health?.disk?.used_percent >= 80) {
    incidents.push({
      title: "Disk pressure",
      description: `${health.disk.used_percent}% of disk is in use.`,
      tone: toneForDisk(health.disk.used_percent),
    });
  }

  state.data.services.forEach((service) => {
    if (service.circuit_open) {
      incidents.push({
        title: `${service.name} circuit open`,
        description: "The circuit breaker is holding the service down.",
        tone: "bad",
      });
    } else if (service.status === "failed") {
      incidents.push({
        title: `${service.name} failed`,
        description: service.last_error || "Service is in failed state.",
        tone: "bad",
      });
    }
  });

  state.data.databases.forEach((database) => {
    if (database.status !== "ready") {
      incidents.push({
        title: `${database.name} ${database.status}`,
        description: `${database.type} database is ${database.status}.`,
        tone: database.status === "failed" ? "bad" : "warn",
      });
    }
  });

  return incidents;
}

function renderIncident(incident) {
  return `
    <div class="list-item">
      <div class="list-item-head">
        <div class="list-item-title">${escapeHtml(incident.title)}</div>
        ${statusBadge(incident.tone, incident.tone)}
      </div>
      <div class="list-item-subtitle">${escapeHtml(incident.description)}</div>
    </div>
  `;
}

function renderActivityItem(event) {
  return `
    <article class="activity-item">
      <div class="activity-head">
        <div class="activity-action">${escapeHtml(event.message)}</div>
        ${event.status ? statusBadge(toneForStatus(event.status), event.status) : `<span class="status-tag">${escapeHtml(event.action)}</span>`}
      </div>
      <p class="mono">${escapeHtml(event.resource_type)} · ${escapeHtml(event.resource_name || event.resource_id)}</p>
      <div class="activity-time">${escapeHtml(formatTimestamp(event.created_at))}</div>
    </article>
  `;
}

function healthCard(label, value, tone, subtitle) {
  return `
    <article class="health-card">
      <div class="detail-label">${escapeHtml(label)}</div>
      <div class="list-item-head">
        <strong>${escapeHtml(value)}</strong>
        ${statusBadge(tone, tone)}
      </div>
      <div class="detail-copy">${escapeHtml(subtitle)}</div>
    </article>
  `;
}

function renderUsageRow(label, used, max) {
  const percent = max > 0 ? Math.min(100, Math.round((used / max) * 100)) : 0;
  return `
    <div class="detail-row">
      <div>
        <strong>${escapeHtml(label)}</strong>
        <div class="detail-copy">${used} / ${max}</div>
      </div>
      <div style="width:120px">
        <div class="usage-meter"><span style="width:${percent}%"></span></div>
      </div>
    </div>
  `;
}

function renderStaticRow(label, value) {
  return `
    <div class="detail-row">
      <strong>${escapeHtml(label)}</strong>
      <span>${escapeHtml(value)}</span>
    </div>
  `;
}

function countStatuses(statuses) {
  if (!statuses.length) {
    return "No resources";
  }
  const counts = statuses.reduce((accumulator, status) => {
    accumulator[status] = (accumulator[status] || 0) + 1;
    return accumulator;
  }, {});
  return Object.entries(counts)
    .slice(0, 3)
    .map(([status, count]) => `${count} ${status}`)
    .join(" · ");
}

function setActiveView(view) {
  state.activeView = sanitizeView(view);
  saveSession();
  window.location.hash = state.activeView;
  render();
}

function sanitizeView(view) {
  const allowed = new Set(["overview", "services", "builds", "databases", "tenant", "activity"]);
  return allowed.has(view) ? view : "overview";
}

function showNotice(message, type) {
  state.notice = { message, type };
}

function normalizeBaseUrl(value) {
  return String(value || "").trim().replace(/\/+$/, "");
}

function toneForStatus(status) {
  switch (status) {
    case "ok":
    case "running":
    case "ready":
    case "succeeded":
    case "active":
      return "good";
    case "deploying":
    case "pending":
    case "provisioning":
    case "stopped":
      return "warn";
    case "failed":
    case "suspended":
      return "bad";
    default:
      return "warn";
  }
}

function toneForService(service) {
  if (service.circuit_open) {
    return "bad";
  }
  return toneForStatus(service.status);
}

function toneForHealth(status) {
  return status === "ok" ? "good" : "bad";
}

function toneForDisk(usedPercent) {
  if (!Number.isFinite(usedPercent)) {
    return "warn";
  }
  if (usedPercent >= 90) {
    return "bad";
  }
  if (usedPercent >= 80) {
    return "warn";
  }
  return "good";
}

function statusBadge(tone, label) {
  return `<span class="status-badge status-${escapeHtml(tone)}"><span class="status-dot"></span>${escapeHtml(label)}</span>`;
}

function formatTimestamp(timestamp) {
  if (!timestamp) {
    return "n/a";
  }
  const date = new Date(Number(timestamp) * 1000);
  if (Number.isNaN(date.getTime())) {
    return "n/a";
  }
  return date.toLocaleString();
}

function shortId(value) {
  if (!value) {
    return "";
  }
  return value.length > 10 ? `${value.slice(0, 10)}...` : value;
}

function trimBaseUrl(value) {
  return value.replace(/^https?:\/\//, "");
}

function truncate(value, max) {
  if (value.length <= max) {
    return value;
  }
  return `${value.slice(0, max - 1)}...`;
}

function safeJsonParse(value) {
  try {
    return JSON.parse(value);
  } catch (_error) {
    return null;
  }
}

function emptyCard(text) {
  return `<article class="kpi-card"><div class="empty-state">${escapeHtml(text)}</div></article>`;
}

function escapeHtml(value) {
  return String(value ?? "")
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}
