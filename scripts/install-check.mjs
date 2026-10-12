import fs from 'node:fs/promises';
import path from 'node:path';
import os from 'node:os';
import {spawnSync} from 'node:child_process';
import assert from 'node:assert/strict';
const root=path.resolve(import.meta.dirname,'..');
const platform={win32:'windows'}[process.platform]||process.platform;
const arch={x64:'amd64',arm64:'arm64'}[process.arch];
const name=`magpie-intelligence-modeltrace-${platform}-${arch}`;
const dir=await fs.mkdtemp(path.join(os.tmpdir(),'quality-install-'));
try{
 const unpack=spawnSync('tar',['-xzf',path.join(root,'dist',name+'.tgz'),'-C',dir],{stdio:'inherit'});assert.equal(unpack.status,0);
 const installed=path.join(dir,name);
 const check=spawnSync(process.execPath,[path.join(root,'scripts/plugin-check.mjs')],{env:{...process.env,PLUGIN_ROOT:installed},stdio:'inherit'});assert.equal(check.status,0);
 const pkg=JSON.parse(await fs.readFile(path.join(installed,'package.json'),'utf8'));assert.equal(pkg.magpie.middleware,'./plugin/gate.middleware.js');
 if(process.env.MAGPIE_BINARY){
  const env={...process.env,HOME:dir,XDG_CONFIG_HOME:path.join(dir,'config'),XDG_CACHE_HOME:path.join(dir,'cache')};
  const registration=spawnSync(process.env.MAGPIE_BINARY,['plugin','add',installed],{env,encoding:'utf8',timeout:90000});
  assert.equal(registration.status,0,registration.stdout+registration.stderr);
  const saved=JSON.parse(await fs.readFile(path.join(dir,'config','magpie','plugins.json'),'utf8'));assert.ok(saved.plugins.some(p=>p.spec===installed));
  const listing=spawnSync(process.env.MAGPIE_BINARY,['plugin'],{env,encoding:'utf8',timeout:90000});assert.equal(listing.status,0,listing.stdout+listing.stderr);assert.match(listing.stdout+listing.stderr,/quality-check|Intelligence Check|intelligence-check/);
  console.log('PASS: real Magpie registers extracted package and recognizes plugin');
 }
 console.log('PASS: extracted release plugin starts without Go, issues management tickets, and cleans up with host');
}finally{await fs.rm(dir,{recursive:true,force:true})}
