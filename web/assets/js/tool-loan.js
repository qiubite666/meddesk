/* ==========================================================================
 * tool-loan.js —— MedDesk 工具箱「贷款计算器」
 *
 * 依赖（只读调用，不修改其文件）：
 *   MD.tools.*  工具箱契约：register / mount / field / select / resultCard
 *   MD.ui.el    通用 DOM 构造（core.js）
 *
 * 分层约定（单向依赖）：
 *   输入层 readXxx()  →  计算层 compute()/buildSchedule()  →  渲染层 renderResult()/renderPlan()
 *   渲染函数之间严禁互相调用，统一由 refresh() 编排。
 *
 * 零外部依赖：无 CDN、无图表库、无网络请求，全部计算在浏览器本地完成。
 * 安全约定：所有用户输入一律使用 textContent 写入，禁止 innerHTML 拼接。
 * ========================================================================== */
(function (window) {
  'use strict';

  var MD = window.MD = window.MD || {};

  /* ============================ 0. 常量 ============================ */

  var PREVIEW_ROWS = 12;        // 计划表默认预览行数（期数多时只渲染前 12 行）
  var MAX_RENDER_MONTHS = 600;  // 按月展开后最多渲染行数，防止极大期数卡顿
  var MAX_RENDER_YEARS = 100;   // 按年展开后最多渲染行数（对应 1200 期上限）
  var MAX_MONTHS = 1200;        // 期限上限：100 年

  var METHOD_EAI = 'eai';       // 等额本息：每期月供固定
  var METHOD_EAP = 'eap';       // 等额本金：每期本金固定、利息递减

  /* ============================ 1. 数值解析与格式化 ============================ */

  // 金额：允许纯数字，也允许「万 / 亿」单位后缀，如 100万 → 1000000
  function parseAmount(raw) {
    var text = String(raw == null ? '' : raw).trim().replace(/[,，\s]/g, '');
    if (!text) return null;
    var scale = 1;
    if (/万$|w$/i.test(text)) { scale = 10000; text = text.replace(/万$|w$/i, ''); }
    else if (/亿$/.test(text)) { scale = 100000000; text = text.replace(/亿$/, ''); }
    if (!text) return null;
    var value = Number(text);
    if (!isFinite(value)) return null;
    return value * scale;
  }

  // 年利率（%）：空值按 0（免息）处理，负数 / 非法字符一律无效
  function parseRate(raw) {
    var text = String(raw == null ? '' : raw).trim().replace(/[,，\s%]/g, '');
    if (!text) return 0;
    var value = Number(text);
    return isFinite(value) ? value : null;
  }

  // 期限（月）：取整后需落在 (0, MAX_MONTHS]
  function parseMonths(raw) {
    var text = String(raw == null ? '' : raw).trim().replace(/[,，\s]/g, '');
    if (!text) return null;
    var value = Number(text);
    if (!isFinite(value)) return null;
    return Math.round(value);
  }

  function group(intPart) {
    return String(intPart).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  }

  // 金额格式化：千分位 + 两位小数；非有限数统一显示为「—」
  function fmtMoney(value) {
    if (value == null || !isFinite(value)) return '—';
    var negative = value < 0;
    var parts = Math.abs(value).toFixed(2).split('.');
    return (negative ? '-' : '') + group(parts[0]) + '.' + parts[1];
  }

  function fmtCount(value) {
    if (value == null || !isFinite(value)) return '—';
    return group(Math.round(value));
  }

  /* ============================ 2. 计算层（纯函数） ============================ */

  function monthlyRate(annualPct) {
    return annualPct / 100 / 12;
  }

  // 等额本息固定月供：M = P·r·(1+r)^n / ((1+r)^n - 1)
  // r = 0 时退化为 P/n，避免 0/0；(1+r)^n 溢出时取其极限值 P·r，避免 Inf/Inf = NaN
  function fixedPayment(principal, rate, months) {
    if (rate === 0) return principal / months;
    var pow = Math.pow(1 + rate, months);
    if (!isFinite(pow) || pow <= 1) return pow <= 1 ? principal / months : principal * rate;
    return principal * rate * pow / (pow - 1);
  }

  // 总利息：等额本息 = M·n - P；等额本金 = P·r·(n+1)/2（等差数列求和，避免逐月累加）
  function totalInterest(principal, rate, months, method) {
    if (method === METHOD_EAP) return principal * rate * (months + 1) / 2;
    return fixedPayment(principal, rate, months) * months - principal;
  }

  // 逐期明细：只生成前 limit 期，期数极大时也不会做无用计算
  function buildSchedule(principal, rate, months, method, limit) {
    var rows = [];
    var balance = principal;
    var perPrincipal = principal / months;
    var fixed = method === METHOD_EAI ? fixedPayment(principal, rate, months) : 0;
    var cap = Math.min(months, limit);
    for (var k = 1; k <= cap; k++) {
      var interest;
      var payment;
      var payPrincipal;
      if (method === METHOD_EAI) {
        interest = balance * rate;
        payment = fixed;
        payPrincipal = payment - interest;
      } else {
        payPrincipal = perPrincipal;
        interest = (principal - (k - 1) * perPrincipal) * rate;
        payment = payPrincipal + interest;
      }
      balance -= payPrincipal;
      // 末期强制归零，抹掉浮点累计误差（避免出现 -0.00 或 0.01 的尾差）
      if (k === months || Math.abs(balance) < 0.005) balance = 0;
      rows.push({ index: k, payment: payment, principal: payPrincipal, interest: interest, balance: balance });
    }
    return rows;
  }

  // 按年汇总：每年最多 12 期，最后一年可能是不足 12 期的零头
  function aggregateByYear(rows) {
    var years = [];
    var current = null;
    rows.forEach(function (row) {
      var year = Math.floor((row.index - 1) / 12) + 1;
      if (!current || current.index !== year) {
        current = { index: year, payment: 0, principal: 0, interest: 0, balance: row.balance, months: 0 };
        years.push(current);
      }
      current.payment += row.payment;
      current.principal += row.principal;
      current.interest += row.interest;
      current.balance = row.balance;
      current.months += 1;
    });
    return years;
  }

  /* ============================ 3. 校验 ============================ */

  // 返回 {ok, model}；model.ok === false 时带上 message 供结果区提示
  function compute(amount, annualPct, months, method) {
    if (amount == null || !isFinite(amount) || amount <= 0) {
      return { ok: false, message: '请输入有效的贷款金额（如 100万 或 1000000）', field: 'amount' };
    }
    if (months == null || !isFinite(months) || months <= 0) {
      return { ok: false, message: '请输入有效的贷款期限（月），如 360', field: 'months' };
    }
    if (months > MAX_MONTHS) {
      return { ok: false, message: '贷款期限最多 ' + MAX_MONTHS + ' 期（100 年）', field: 'months' };
    }
    if (annualPct == null || !isFinite(annualPct)) {
      return { ok: false, message: '年利率格式不正确，请填写数字（如 4.9）', field: 'rate' };
    }
    if (annualPct < 0) {
      return { ok: false, message: '年利率不能为负数', field: 'rate' };
    }

    var rate = monthlyRate(annualPct);
    var interest = totalInterest(amount, rate, months, method);
    var total = amount + interest;
    var firstPayment = method === METHOD_EAI
      ? fixedPayment(amount, rate, months)
      : amount / months + amount * rate;
    var lastPayment = method === METHOD_EAI
      ? fixedPayment(amount, rate, months)
      : amount / months + (amount / months) * rate;
    var step = method === METHOD_EAP ? (amount / months) * rate : 0;

    return {
      ok: true,
      method: method,
      principal: amount,
      annualPct: annualPct,
      rate: rate,
      months: months,
      firstPayment: firstPayment,
      lastPayment: lastPayment,
      step: step,
      interest: interest,
      total: total
    };
  }

  /* ============================ 4. 表格构造 ============================ */

  var CELL_STYLE = { padding: '8px 6px', borderBottom: '1px solid var(--border)', whiteSpace: 'nowrap' };

  function headCell(text, alignLeft) {
    return MD.ui.el('th', {
      scope: 'col',
      text: text,
      style: {
        padding: '8px 6px',
        textAlign: alignLeft ? 'left' : 'right',
        whiteSpace: 'nowrap',
        color: 'var(--text-dim)',
        fontWeight: '700',
        borderBottom: '1px solid var(--border)'
      }
    });
  }

  function bodyCell(text, alignLeft) {
    return MD.ui.el('td', {
      text: text,
      style: {
        padding: CELL_STYLE.padding,
        borderBottom: CELL_STYLE.borderBottom,
        whiteSpace: CELL_STYLE.whiteSpace,
        textAlign: alignLeft ? 'left' : 'right',
        fontVariantNumeric: 'tabular-nums'
      }
    });
  }

  function buildTable(columns, rows) {
    var table = MD.ui.el('table', { style: { width: '100%', borderCollapse: 'collapse', fontSize: '13px' } });
    var headRow = MD.ui.el('tr');
    columns.forEach(function (text, index) { headRow.appendChild(headCell(text, index === 0)); });
    var thead = MD.ui.el('thead', null, headRow);
    var tbody = MD.ui.el('tbody');
    rows.forEach(function (cells) {
      var tr = MD.ui.el('tr');
      cells.forEach(function (text, index) { tr.appendChild(bodyCell(text, index === 0)); });
      tbody.appendChild(tr);
    });
    table.appendChild(thead);
    table.appendChild(tbody);
    return MD.ui.el('div', { style: { overflowX: 'auto', WebkitOverflowScrolling: 'touch' } }, table);
  }

  /* ============================ 5. 渲染 ============================ */

  function render(view) {
    var content = MD.tools.mount(view, 'loan');

    var state = { method: METHOD_EAI, granularity: 'month', expanded: false };
    var amountInput = null;
    var rateInput = null;
    var monthsInput = null;
    var amountField = null;
    var rateField = null;
    var monthsField = null;
    var resultHost = MD.ui.el('div', { class: 'stack' });
    var planHost = MD.ui.el('div', { class: 'card' });

    function setError(field, message) {
      if (field && typeof field.setError === 'function') field.setError(message || '');
    }

    function clearErrors() {
      setError(amountField, '');
      setError(rateField, '');
      setError(monthsField, '');
    }

    /* ---- 5.1 输入区 ---- */

    amountField = MD.tools.field({
      label: '贷款金额',
      unit: '元',
      value: '',
      placeholder: '如 1000000 或 100万',
      hint: '支持「万 / 亿」单位，例如 100万 = 1000000 元',
      onInput: function () { state.expanded = false; refresh(); }
    });
    amountInput = amountField.input;
    // 金额允许带单位后缀，number 输入框会把「100万」清空，故降级为文本输入
    if (amountInput) {
      amountInput.type = 'text';
      amountInput.setAttribute('inputmode', 'decimal');
      amountInput.setAttribute('autocomplete', 'off');
    }

    rateField = MD.tools.field({
      label: '年利率',
      unit: '%',
      value: '',
      placeholder: '如 4.9',
      step: '0.01',
      min: '0',
      hint: '留空或填 0 表示免息',
      onInput: function () { state.expanded = false; refresh(); }
    });
    rateInput = rateField.input;

    monthsField = MD.tools.field({
      label: '贷款期限',
      unit: '月',
      value: '',
      placeholder: '如 360',
      step: '1',
      min: '1',
      hint: '最多 ' + MAX_MONTHS + ' 期（100 年）',
      onInput: function () { state.expanded = false; refresh(); }
    });
    monthsInput = monthsField.input;

    var methodField = MD.tools.select({
      label: '还款方式',
      value: METHOD_EAI,
      options: [
        { value: METHOD_EAI, label: '等额本息（月供固定）' },
        { value: METHOD_EAP, label: '等额本金（月供递减）' }
      ],
      onChange: function (value) { state.method = value; state.expanded = false; refresh(); }
    });

    var form = MD.ui.el('div', { class: 'card', style: { display: 'grid', gap: '12px' } });
    form.appendChild(amountField.row);
    form.appendChild(rateField.row);
    form.appendChild(monthsField.row);
    form.appendChild(methodField.row);

    /* ---- 5.2 计划表粒度切换 ---- */

    var seg = MD.ui.el('div', { class: 'seg', role: 'group', 'aria-label': '还款计划粒度' });
    [['month', '按月'], ['year', '按年']].forEach(function (item) {
      var key = item[0];
      var active = state.granularity === key;
      var button = MD.ui.el('button', {
        class: 'seg__item' + (active ? ' is-active' : ''),
        type: 'button',
        text: item[1],
        'aria-pressed': active ? 'true' : 'false',
        onClick: function () {
          if (state.granularity === key) return;
          state.granularity = key;
          state.expanded = false;
          Array.prototype.forEach.call(seg.children, function (child) {
            var on = child.dataset.granularity === key;
            child.classList.toggle('is-active', on);
            child.setAttribute('aria-pressed', on ? 'true' : 'false');
          });
          refresh();
        }
      });
      button.dataset.granularity = key;
      seg.appendChild(button);
    });

    /* ---- 5.3 结果区 ---- */

    function renderResult(model) {
      MD.ui.clear(resultHost);
      if (!model.ok) {
        resultHost.appendChild(MD.tools.resultCard({
          title: '计算结果',
          tone: 'warn',
          items: [
            { label: '月供', value: '—', unit: '元', strong: true },
            { label: '支付利息总额', value: '—', unit: '元' },
            { label: '本息合计', value: '—', unit: '元' }
          ]
        }));
        resultHost.appendChild(MD.ui.el('p', { class: 'muted', style: { margin: '0', fontSize: '13px' }, text: model.message }));
        return;
      }

      var items;
      if (model.method === METHOD_EAI) {
        items = [
          { label: '月供（每期固定）', value: fmtMoney(model.firstPayment), unit: '元', strong: true },
          { label: '支付利息总额', value: fmtMoney(model.interest), unit: '元' },
          { label: '本息合计', value: fmtMoney(model.total), unit: '元' },
          { label: '还款期数', value: fmtCount(model.months), unit: '期' }
        ];
      } else {
        items = [
          { label: '首月月供', value: fmtMoney(model.firstPayment), unit: '元', strong: true },
          { label: '每月递减', value: fmtMoney(model.step), unit: '元' },
          { label: '末月月供', value: fmtMoney(model.lastPayment), unit: '元' },
          { label: '支付利息总额', value: fmtMoney(model.interest), unit: '元' },
          { label: '本息合计', value: fmtMoney(model.total), unit: '元' }
        ];
      }
      resultHost.appendChild(MD.tools.resultCard({ title: '计算结果', tone: '', items: items }));
      resultHost.appendChild(MD.ui.el('p', {
        class: 'muted',
        style: { margin: '0', fontSize: '13px' },
        text: model.method === METHOD_EAI
          ? '等额本息：每期月供固定，前期利息占比高、本金占比低。'
          : '等额本金：每期本金固定，月供逐月递减 ' + fmtMoney(model.step) + ' 元，前期压力较大、总利息更少。'
      }));
    }

    /* ---- 5.4 还款计划表 ---- */

    function planRows(model, rowCount) {
      var byYear = state.granularity === 'year';
      var monthlyLimit = byYear ? rowCount * 12 : rowCount;
      var monthly = buildSchedule(model.principal, model.rate, model.months, model.method, monthlyLimit);
      if (!byYear) return monthly;
      return aggregateByYear(monthly).slice(0, rowCount);
    }

    function renderPlan(model) {
      MD.ui.clear(planHost);

      var head = MD.ui.el('div', { class: 'row row--wrap', style: { marginBottom: '10px' } }, [
        MD.ui.el('h3', { class: 'section-title', style: { margin: '0', flex: '1 1 auto' }, text: '还款计划' }),
        seg
      ]);
      planHost.appendChild(head);

      if (!model.ok) {
        planHost.appendChild(MD.ui.el('div', { class: 'empty' }, [
          MD.icon('banknote'),
          MD.ui.el('p', { class: 'muted', style: { margin: '0' }, text: '填写贷款金额与期限后，这里将显示逐期明细' })
        ]));
        return;
      }

      var totalRows = state.granularity === 'year'
        ? Math.ceil(model.months / 12)
        : model.months;
      var cap = state.granularity === 'year' ? MAX_RENDER_YEARS : MAX_RENDER_MONTHS;
      var rowCount = state.expanded ? Math.min(totalRows, cap) : Math.min(totalRows, PREVIEW_ROWS);

      var rows = planRows(model, rowCount);
      var byYear = state.granularity === 'year';
      var columns = byYear
        ? ['年份', '年还款额', '年本金', '年利息', '剩余本金']
        : ['期数', '月供', '本金', '利息', '剩余本金'];
      var body = rows.map(function (row) {
        return [
          byYear ? '第 ' + row.index + ' 年' : '第 ' + row.index + ' 期',
          fmtMoney(row.payment),
          fmtMoney(row.principal),
          fmtMoney(row.interest),
          fmtMoney(row.balance)
        ];
      });

      planHost.appendChild(buildTable(columns, body));

      var foot = MD.ui.el('div', { class: 'row row--wrap', style: { marginTop: '10px' } });
      foot.appendChild(MD.ui.el('span', {
        class: 'muted',
        style: { fontSize: '13px', flex: '1 1 auto' },
        text: '共 ' + fmtCount(model.months) + ' 期，当前展示前 ' + fmtCount(rows.length) + (byYear ? ' 年' : ' 期')
      }));

      if (totalRows > rows.length) {
        var canExpand = rows.length < cap;
        foot.appendChild(MD.ui.el('button', {
          class: 'btn btn-ghost',
          type: 'button',
          text: state.expanded ? '收起' : '展开全部',
          onClick: function () {
            state.expanded = !state.expanded;
            MD.haptic.tap();
            renderPlan(model);
          }
        }));
        if (!canExpand) {
          planHost.appendChild(MD.ui.el('p', {
            class: 'muted',
            style: { margin: '8px 0 0', fontSize: '12px' },
            text: '为保证流畅，最多展示前 ' + fmtCount(cap) + (byYear ? ' 年' : ' 期') + '，完整明细可按需分段查看。'
          }));
        }
      } else if (state.expanded && totalRows > PREVIEW_ROWS) {
        foot.appendChild(MD.ui.el('button', {
          class: 'btn btn-ghost',
          type: 'button',
          text: '收起',
          onClick: function () {
            state.expanded = false;
            MD.haptic.tap();
            renderPlan(model);
          }
        }));
      }
      planHost.appendChild(foot);
    }

    /* ---- 5.5 编排 ---- */

    function refresh() {
      clearErrors();
      var model = compute(
        parseAmount(amountInput ? amountInput.value : ''),
        parseRate(rateInput ? rateInput.value : ''),
        parseMonths(monthsInput ? monthsInput.value : ''),
        state.method
      );
      if (!model.ok) {
        if (model.field === 'amount') setError(amountField, model.message);
        else if (model.field === 'rate') setError(rateField, model.message);
        else if (model.field === 'months') setError(monthsField, model.message);
      }
      renderResult(model);
      renderPlan(model);
    }

    content.appendChild(form);
    content.appendChild(resultHost);
    content.appendChild(planHost);
    refresh();
  }

  /* ============================ 6. 注册 ============================ */

  MD.tools.register({
    id: 'loan',
    title: '贷款计算器',
    subtitle: '等额本息与等额本金',
    icon: 'banknote',
    category: '计算工具',
    order: 20,
    render: render
  });
})(typeof window !== 'undefined' ? window : this);
