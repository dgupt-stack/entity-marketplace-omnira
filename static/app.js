const state = { listings: [], category: "All", search: "" };
const grid = document.querySelector("#listingGrid");
const template = document.querySelector("#listingTemplate");
const summary = document.querySelector("#marketSummary");
const filters = document.querySelector("#filters");
const emptyState = document.querySelector("#emptyState");
const sellDialog = document.querySelector("#sellDialog");
const diagnosticsDialog = document.querySelector("#diagnosticsDialog");
const form = document.querySelector("#sellForm");

const glyphs = { Cameras: "◉", Home: "∪", Music: "⌁", Outdoors: "△", Books: "≡", Other: "◇" };

function money(cents) {
  return new Intl.NumberFormat("en-US", { style: "currency", currency: "USD", maximumFractionDigits: cents % 100 ? 2 : 0 }).format(cents / 100);
}

function filteredListings() {
  const search = state.search.toLowerCase();
  return state.listings.filter((item) => {
    const categoryMatch = state.category === "All" || item.category === state.category;
    const text = `${item.title} ${item.description} ${item.seller} ${item.category}`.toLowerCase();
    return categoryMatch && (!search || text.includes(search));
  });
}

function renderFilters() {
  const categories = ["All", ...new Set(state.listings.map((item) => item.category))];
  filters.replaceChildren(...categories.map((category) => {
    const button = document.createElement("button");
    button.className = `filter-button${state.category === category ? " active" : ""}`;
    button.textContent = category;
    button.addEventListener("click", () => { state.category = category; renderFilters(); renderListings(); });
    return button;
  }));
}

function renderListings() {
  const listings = filteredListings();
  grid.replaceChildren();
  for (const item of listings) {
    const card = template.content.firstElementChild.cloneNode(true);
    card.dataset.id = item.id;
    card.querySelector(".category-glyph").textContent = glyphs[item.category] || glyphs.Other;
    const badge = card.querySelector(".status-badge");
    badge.textContent = item.status;
    badge.classList.add(item.status);
    card.querySelector(".category").textContent = item.category;
    card.querySelector(".price").textContent = money(item.priceCents);
    card.querySelector("h3").textContent = item.title;
    card.querySelector(".description").textContent = item.description;
    card.querySelector(".seller-avatar").textContent = item.seller.slice(0, 1).toUpperCase();
    card.querySelector(".seller-name").textContent = item.seller;
    card.querySelector(".delete-button").addEventListener("click", () => removeListing(item));
    grid.append(card);
  }
  summary.textContent = `${listings.length} of ${state.listings.length} thoughtful finds`;
  emptyState.hidden = listings.length > 0;
  grid.hidden = listings.length === 0;
}

async function api(path, options = {}) {
  const response = await fetch(path, { headers: { "Content-Type": "application/json", ...(options.headers || {}) }, ...options });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `Request failed (${response.status})`);
  return body;
}

async function refresh() {
  try {
    const result = await api("/api/listings");
    state.listings = result.listings;
    document.querySelector("#liveStatus").textContent = `Entity live · revision ${result.revision}`;
    document.querySelector(".live-pill").classList.remove("offline");
    renderFilters();
    renderListings();
  } catch (error) {
    document.querySelector("#liveStatus").textContent = "Entity Service unavailable";
    document.querySelector(".live-pill").classList.add("offline");
    summary.textContent = error.message;
  }
}

async function removeListing(item) {
  if (!window.confirm(`Remove “${item.title}” from this demo marketplace?`)) return;
  try {
    await api(`/api/listings/${encodeURIComponent(item.id)}`, { method: "DELETE" });
    await refresh();
  } catch (error) {
    window.alert(error.message);
  }
}

form.addEventListener("submit", async (event) => {
  event.preventDefault();
  const values = new FormData(form);
  const message = document.querySelector("#formMessage");
  message.textContent = "Publishing to Entity Service…";
  try {
    await api("/api/listings", {
      method: "POST",
      body: JSON.stringify({
        title: values.get("title"), description: values.get("description"),
        category: values.get("category"), priceCents: Math.round(Number(values.get("price")) * 100),
        seller: values.get("seller"), status: "available",
      }),
    });
    form.reset();
    message.textContent = "";
    sellDialog.close();
    await refresh();
    document.querySelector("#market").scrollIntoView({ behavior: "smooth" });
  } catch (error) {
    message.textContent = error.message;
  }
});

async function showDiagnostics() {
  diagnosticsDialog.showModal();
  const content = document.querySelector("#diagnosticsContent");
  content.innerHTML = "<p>Reading the live Entity block…</p>";
  try {
    const proof = await api("/_omnira/storage");
    const metrics = proof.metrics;
    content.replaceChildren();
    const banner = document.createElement("div");
    banner.className = "proof-banner";
    banner.innerHTML = "<i></i><span>Strict Entity-only · ready</span>";
    content.append(banner);
    const metricGrid = document.createElement("div");
    metricGrid.className = "metric-grid";
    const items = [
      ["State generation", proof.stateGeneration], ["State size", `${proof.stateBytes.toLocaleString()} bytes`],
      ["Marketplace revision", proof.marketplaceRevision], ["Listings", proof.listingCount],
      ["Last Entity read", `${metrics.lastReadMs.toFixed(1)} ms`], ["CAS conflicts", metrics.casConflicts],
      ["Durable local disk", String(proof.durableLocalDisk)], ["External database", String(proof.externalDatabase)],
    ];
    for (const [label, value] of items) {
      const metric = document.createElement("div"); metric.className = "metric";
      const small = document.createElement("small"); small.textContent = label;
      const strong = document.createElement("strong"); strong.textContent = value;
      metric.append(small, strong); metricGrid.append(metric);
    }
    content.append(metricGrid);
    const note = document.createElement("p"); note.className = "limitations";
    note.textContent = "Baseline constraint: every write replaces one CAS-protected JSON block, capped at 512 KiB and 250 listings. This is the behavior we will measure before partitioning the model.";
    content.append(note);
  } catch (error) {
    content.textContent = error.message;
  }
}

function closeSellDialog() {
  form.reset();
  document.querySelector("#formMessage").textContent = "";
  sellDialog.close();
}

document.querySelector("#sellButton").addEventListener("click", () => sellDialog.showModal());
document.querySelector("#cancelSell").addEventListener("click", closeSellDialog);
document.querySelector("#closeSell").addEventListener("click", closeSellDialog);
document.querySelector("#diagnosticsButton").addEventListener("click", showDiagnostics);
document.querySelector("#openDiagnostics").addEventListener("click", showDiagnostics);
document.querySelector("#closeDiagnostics").addEventListener("click", () => diagnosticsDialog.close());
document.querySelector("#searchInput").addEventListener("input", (event) => { state.search = event.target.value; renderListings(); });

refresh();
