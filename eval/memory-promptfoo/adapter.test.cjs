const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const Provider = require('./provider.cjs');
const score = require('./assertion.cjs');
const { prepare } = require('./prepare.cjs');
test('import keeps group isolation, failures, original accounting, and no reruns', async t => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'memory-promptfoo-'));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const report = { mode: 'live', suite_sha256: 'hash', results: [
    { case: 'cancel', repeat: 1, group: 'memory', status: 'ability_failed', checks: [{stage:'update',applicable:true,pass:false,detail:'cancel missed'}], calls:[{input_tokens:8,output_tokens:2,model:'test'}] },
    { case: 'cancel', repeat: 1, group: 'no-memory', status: 'passed', checks: [{stage:'update',applicable:false,pass:false}] }
  ] };
  const file = path.join(dir,'results.json'); fs.writeFileSync(file, JSON.stringify(report));
  const config = prepare(file,path.join(dir,'config.json'));
  assert.equal(config.tests.length,1); assert.equal(config.providers.length,2);
  const provider = new Provider({config:{report:file,group:'memory'}});
  const out = await provider.callApi('',{vars:{case_id:'cancel',repeat:1}});
  assert.equal(score(out.output).pass,false); assert.equal(out.metadata.originalTokenUsage.total,10);
  assert.equal(out.tokenUsage,undefined); // Import must not pretend to consume model tokens.
  assert.equal((await provider.callApi('',{vars:{case_id:'missing',repeat:1}})).error.includes('did not run'),true);
  const noMemory = await new Provider({config:{report:file,group:'no-memory'}}).callApi('',{vars:{case_id:'cancel',repeat:1}});
  assert.equal(score(noMemory.output).pass,true);
  assert.equal(JSON.parse(noMemory.output).stage_scores.update.total,0);
  report.results[0].status='budget_stopped'; fs.writeFileSync(file,JSON.stringify(report));
  assert.match((await new Provider({config:{report:file,group:'memory'}}).callApi('',{vars:{case_id:'cancel',repeat:1}})).error,/budget_stopped/);
  assert.throws(()=>Provider.validate({...report,results:[...report.results,report.results[0]]}),/duplicate/);
});
test('offline completion is never presented as model capability',()=>{
  assert.match(score({mode:'offline',status:'offline_validated'}).reason,/not a model capability/);
  assert.equal(score({mode:'offline',status:'service_error'}).pass,false);
});
test('viewer preload binds only IPv4 loopback', async()=>{
  const { execFileSync } = require('node:child_process');
  const code = "const s=require('node:net').createServer();s.listen(0,()=>{console.log(s.address().address);s.close()})";
  assert.equal(execFileSync(process.execPath,['--require',path.join(__dirname,'bind-local.cjs'),'-e',code],{encoding:'utf8'}).trim(),'127.0.0.1');
});
