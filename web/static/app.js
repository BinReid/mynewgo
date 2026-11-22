'use strict';

document.addEventListener('click', (event) => {
  if (!event.target.closest('[data-open-form]')) return;
  const form = document.getElementById('add-site');
  if (!form) return;
  event.preventDefault();
  form.open = true;
  form.querySelector('input').focus();
});

let hoveredChart = null;
let drawChart = null;
let draggedRow = null;
let savingOrder = false;
let requestVersion = 0;
let searchTimer;
const timeLabel = new Intl.DateTimeFormat('ru', { hour: '2-digit', minute: '2-digit' });

function initChart() {
  drawChart = null;
  const canvas = document.querySelector('.latency-chart');
  if (!canvas) return;
  const checks = JSON.parse(document.querySelector('.chart-data').textContent);
  const window = document.querySelector('.chart-window').dataset;
  const end = Number(window.end);
  const start = Math.max(Number(window.start), Math.min(checks[0].at - 10, end - 300));
  const breakInterval = Number(window.interval) * 2;
  const peak = checks.reduce((max, check) => check.ok ? Math.max(max, check.latency) : max, 0);
  const step = Math.pow(10, Math.floor(Math.log10(Math.max(peak, 20))));
  const ceiling = Math.ceil(Math.max(peak * 1.12, 20) / step) * step;
  const context = canvas.getContext('2d');
  if (!context) return;
  const left = 43;
  const right = 23;
  const top = 15;
  const bottom = 29;
  let selected = null;
  const offsetX = (width, at) => left + ((at - start) / (end - start)) * (width - left - right);
  drawChart = () => {
    const { width, height } = canvas.getBoundingClientRect();
    if (!width || !height) return;
    const ratio = globalThis.devicePixelRatio || 1;
    canvas.width = Math.round(width * ratio);
    canvas.height = Math.round(height * ratio);
    context.setTransform(ratio, 0, 0, ratio, 0, 0);
    context.font = '11px Inter, Segoe UI, sans-serif';
    const plotHeight = height - top - bottom;
    const y = (latency) => top + (1 - latency / ceiling) * plotHeight;
    for (let i = 0; i <= 4; i++) {
      const latency = ceiling * i / 4;
      const position = y(latency);
      context.strokeStyle = '#293447';
      context.beginPath();
      context.moveTo(left, position);
      context.lineTo(width - right, position);
      context.stroke();
      context.fillStyle = '#a1acc0';
      context.textAlign = 'right';
      context.fillText(Math.round(latency), left - 8, position + 4);
    }
    const ticks = width < 480 ? 3 : 5;
    context.textAlign = 'center';
    for (let i = 0; i <= ticks; i++) {
      const at = start + (i / ticks) * (end - start);
      context.fillText(timeLabel.format(at * 1000), offsetX(width, at), height - 7);
    }
    context.strokeStyle = '#64e0b2';
    context.lineWidth = 2;
    context.beginPath();
    let previous = null;
    for (const check of checks) {
      if (!check.ok) { previous = null; continue; }
      const x = offsetX(width, check.at);
      if (previous && check.at - previous.at <= breakInterval) context.lineTo(x, y(check.latency));
      else context.moveTo(x, y(check.latency));
      previous = check;
    }
    context.stroke();
    for (const check of checks) {
      if (checks.length > 180 && check.ok && check !== selected) continue;
      context.fillStyle = check.ok ? '#64e0b2' : '#ff9398';
      context.beginPath();
      context.arc(offsetX(width, check.at), check.ok ? y(check.latency) : top + plotHeight, check === selected ? 5 : 3, 0, Math.PI * 2);
      context.fill();
    }
  };
  canvas.addEventListener('pointermove', (event) => {
    hoveredChart = canvas;
    const { left: canvasLeft, width } = canvas.getBoundingClientRect();
    const x = event.clientX - canvasLeft;
    const closest = checks.reduce((nearest, check) => (
      Math.abs(offsetX(width, check.at) - x) < Math.abs(offsetX(width, nearest.at) - x) ? check : nearest
    ));
    const next = Math.abs(offsetX(width, closest.at) - x) < 12 ? closest : null;
    if (next === selected) return;
    selected = next;
    canvas.title = next ? `${timeLabel.format(next.at * 1000)} · ${next.ok ? `${next.latency} мс` : next.problem}` : '';
    drawChart();
  });
  canvas.addEventListener('pointerleave', () => {
    hoveredChart = null;
    selected = null;
    canvas.title = '';
    drawChart();
  });
  drawChart();
}

function clearDropTarget() {
  document.querySelectorAll('.drag-over-top, .drag-over-bottom').forEach((row) => {
    row.classList.remove('drag-over-top', 'drag-over-bottom');
  });
}

async function moveSite(row, target, after) {
  if (savingOrder || row === target) return;
  savingOrder = true;
  requestVersion++;
  const message = document.getElementById('order-message');
  message.textContent = 'Сохраняем порядок…';
  try {
    const response = await fetch('/sites/order', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: Number(row.dataset.id), target: Number(target.dataset.id), after }),
    });
    if (!response.ok) throw new Error('save failed');
    target.insertAdjacentElement(after ? 'afterend' : 'beforebegin', row);
    message.textContent = 'Порядок сохранён';
  } catch {
    message.textContent = 'Не удалось сохранить порядок. Повторите перемещение.';
  } finally {
    savingOrder = false;
    requestVersion++;
  }
}

document.addEventListener('dragstart', (event) => {
  const handle = event.target.closest('.grip');
  if (!handle || savingOrder) { event.preventDefault(); return; }
  draggedRow = handle.closest('[data-id]');
  draggedRow.classList.add('dragging');
  requestVersion++;
  event.dataTransfer.effectAllowed = 'move';
  event.dataTransfer.setData('text/plain', draggedRow.dataset.id);
});

document.addEventListener('dragover', (event) => {
  const target = event.target.closest('[data-id]');
  if (!draggedRow || !target || target === draggedRow || savingOrder) return;
  event.preventDefault();
  event.dataTransfer.dropEffect = 'move';
  clearDropTarget();
  const bounds = target.getBoundingClientRect();
  target.classList.add(event.clientY > bounds.top + bounds.height / 2 ? 'drag-over-bottom' : 'drag-over-top');
});

document.addEventListener('drop', (event) => {
  const target = event.target.closest('[data-id]');
  if (!draggedRow || !target || target === draggedRow) return;
  event.preventDefault();
  const bounds = target.getBoundingClientRect();
  moveSite(draggedRow, target, event.clientY > bounds.top + bounds.height / 2);
  clearDropTarget();
});

document.addEventListener('dragend', () => {
  draggedRow?.classList.remove('dragging');
  draggedRow = null;
  clearDropTarget();
});

document.addEventListener('keydown', (event) => {
  const handle = event.target.closest('.grip');
  if (!handle || !['ArrowUp', 'ArrowDown'].includes(event.key)) return;
  event.preventDefault();
  const rows = [...document.querySelectorAll('[data-id]')];
  const row = handle.closest('[data-id]');
  const after = event.key === 'ArrowDown';
  const target = rows[rows.indexOf(row) + (after ? 1 : -1)];
  if (target) moveSite(row, target, after);
});

const searchForm = document.getElementById('site-search');
const searchInput = searchForm?.querySelector('input[name="q"]');

function searchSites() {
  clearTimeout(searchTimer);
  const address = new URL(location.href);
  if (searchInput.value.trim()) address.searchParams.set('q', searchInput.value.trim());
  else address.searchParams.delete('q');
  history.replaceState(null, '', address.pathname + address.search);
  document.querySelector('.add-form').action = address.pathname + address.search;
  updateContent(true);
}

searchInput?.addEventListener('input', () => {
  requestVersion++;
  clearTimeout(searchTimer);
  searchTimer = setTimeout(searchSites, 200);
});
searchForm?.addEventListener('submit', (event) => {
  event.preventDefault();
  searchSites();
});
searchForm?.querySelector('.search-reset').addEventListener('click', (event) => {
  event.preventDefault();
  searchInput.value = '';
  searchSites();
  searchInput.focus();
});

initChart();
globalThis.addEventListener('resize', () => drawChart?.());

async function updateContent(searching = false) {
  try {
    const current = document.querySelector('[data-refresh]');
    if (!current || document.hidden || hoveredChart || draggedRow || savingOrder || current.contains(document.activeElement)) return;
    const version = ++requestVersion;
    const response = await fetch(location.pathname + location.search, { cache: 'no-store' });
    if (!response.ok) throw new Error('refresh failed');
    const documentCopy = new DOMParser().parseFromString(await response.text(), 'text/html');
    const next = documentCopy.getElementById(current.id);
    if (next && version === requestVersion && !document.hidden && !hoveredChart && !draggedRow && !savingOrder && !current.contains(document.activeElement) && current.innerHTML !== next.innerHTML) {
      current.replaceWith(next);
      initChart();
    }
  } catch {
    if (searching) document.getElementById('order-message').textContent = 'Не удалось выполнить поиск. Повторите запрос.';
  }
}

async function refresh() {
  await updateContent();
  globalThis.setTimeout(refresh, 5000);
}
globalThis.setTimeout(refresh, 5000);
