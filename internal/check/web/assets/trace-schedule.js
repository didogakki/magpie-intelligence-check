import { $, text, dateTime } from './ui.js';

const outcomes = {
  started: '已启动', reserved: '准备中', imported: '已导入旧状态', target_missing: '监测目标已删除，已跳过',
  target_disabled: '监测已关闭，已跳过', gateway_unconfigured: '网关未配置，已跳过', running: '该模型正在检测，已跳过',
  daily_cap: '达到每日次数上限，已跳过', budget: '今日预算不足，已跳过', trigger_error: '启动失败', interrupted: '服务重启，本次不重试',
};
const runStates = { running: '进行中', completed: '已完成', unavailable: '本轮不可用', records_cleared: '记录已清理' };
const classes = { match: '一致', mismatch: '不一致', 529: 'HTTP 529', transient: '临时故障', unknown: '未得出结果' };
const cadenceLabel = minutes => minutes % 60 ? `${minutes} 分钟` : `${minutes / 60} 小时`;

let dirty = false, rendered, api;
const form = $('#scheduleForm');
form.oninput = () => { dirty = true; };

function statusLine(status) {
  if (!status?.outcome) return '尚未运行';
  const result = status.class ? ` / 指纹结果 ${classes[status.class] || status.class}${status.prediction ? '（' + status.prediction + '）' : ''} / 连续一致 ${status.streak} / 当前间隔 ${cadenceLabel(status.cadence_minutes)}` : '';
  const outcome = status.outcome === 'started' && runStates[status.run_state] ? `已启动 / ${runStates[status.run_state]}` : outcomes[status.outcome] || status.outcome;
  return `${outcome} / ${dateTime(status.last_at)}${result}${status.detail ? ' / ' + status.detail : ''}`;
}

export function renderTraceSchedule(data, hooks) {
  api = hooks;
  const schedule = data.state.trace_schedule;
  const statuses = new Map((data.trace_schedule_status || []).map(item => [item.model, item]));
  const models = [...new Set([...Object.keys(data.state.targets), ...Object.keys(schedule.models)])];
  const scheduled = Object.keys(schedule.models).length;
  $('#scheduleSummary').textContent = schedule.enabled ? `已启用 / ${scheduled} 个模型` : '默认关闭';
  if (dirty) return;
  const signature = JSON.stringify([schedule, models, [...statuses.values()].map(s => [s.outcome, s.run_state, s.last_at, s.next_at, s.paused, s.today_runs, s.streak, s.class])]);
  if (signature === rendered) return;
  rendered = signature;
  form.elements.enabled.checked = schedule.enabled;
  const rows = models.map(model => {
    const plan = schedule.models[model], status = statuses.get(model);
    const row = text('div', '', 'schedule-row');
    const pick = document.createElement('input');
    pick.type = 'checkbox'; pick.name = 'pick'; pick.value = model; pick.checked = Boolean(plan);
    pick.setAttribute('aria-label', `定时检测 ${model}`);
    const name = text('label', '', 'schedule-model');
    name.append(pick, text('span', model, 'model-id'));
    const interval = text('span', '15 分钟起，随连续一致增至 4 小时', 'meta');
    interval.dataset.expect = plan?.expect || '';
    const cap = document.createElement('input');
    cap.type = 'number'; cap.name = 'cap'; cap.min = 1; cap.max = 24; cap.value = plan?.daily_cap || 8;
    cap.setAttribute('aria-label', `${model} 每日最多定时次数`);
    const info = text('p', '', 'schedule-status meta');
    info.textContent = statusLine(status) + (status?.paused ? ` / 已暂停自动检测：${status.pause_reason}` : '') + (schedule.enabled && plan && !status?.paused && status?.next_at ? ` / 下次 ${dateTime(status.next_at)}` : '') + (plan ? ` / 今日 ${status?.today_runs || 0}/${plan.daily_cap} / ${status?.note || '指纹结果，不证明物理身份'}` : '');
    row.append(name, interval, cap, info);
    return row;
  });
  $('#scheduleModels').replaceChildren(...(rows.length ? rows : [text('p', '请先在“模型监测”添加监测目标', 'muted')]));
}

form.onsubmit = async event => {
  event.preventDefault();
  const button = form.querySelector('button'), output = $('#scheduleMessage');
  button.disabled = true;
  try {
    const picked = {};
    for (const row of form.querySelectorAll('.schedule-row')) {
      const pick = row.querySelector('[name=pick]');
      if (!pick.checked) continue;
      picked[pick.value] = { interval_minutes: 15, daily_cap: Number(row.querySelector('[name=cap]').value) };
      const expect = row.querySelector('[data-expect]').dataset.expect;
      if (expect) picked[pick.value].expect = expect;
    }
    await api.request('/api/trace-schedule', { enabled: form.elements.enabled.checked, models: picked });
    dirty = false; rendered = undefined;
    output.textContent = '定时设置已保存'; output.className = 'inline-state success';
    await api.refresh();
  } catch (error) { output.textContent = error.message; output.className = 'inline-state error'; }
  finally { button.disabled = false; }
};
