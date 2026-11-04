'use strict';
module.exports = output => {
  const r = typeof output === 'string' ? JSON.parse(output) : output;
  if (r.mode === 'offline') return { pass: r.status === 'offline_validated', score: r.status === 'offline_validated' ? 1 : 0,
    reason: 'OFFLINE: protocol completion only; not a model capability score' };
  const bad = r.checks.filter(c => c.applicable && !c.pass);
  return { pass: r.status === 'passed' && !bad.length, score: r.status === 'passed' && !bad.length ? 1 : 0,
    reason: bad.length ? bad.map(c => `${c.stage}/${c.subject}: ${c.detail}`).join('; ') :
      'Applicable checks passed; N/A stages excluded. Overall group pass rates are not comparable.' };
};
