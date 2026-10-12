import { spawnSync } from 'node:child_process';
import { mkdir, copyFile, readFile, writeFile, cp } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import path from 'node:path';

const root=path.resolve(import.meta.dirname,'..');
const platforms=[['linux','amd64'],['linux','arm64'],['darwin','amd64'],['darwin','arm64'],['windows','amd64']];
const {version}=JSON.parse(await readFile(path.join(root,'package.json'),'utf8'));
const manifest={version,artifacts:[]};
for(const [goos,goarch] of platforms){
 const name=`quality-check-${goos}-${goarch}${goos==='windows'?'.exe':''}`;
 const output=path.join(root,'dist',name);await mkdir(path.dirname(output),{recursive:true});
 const result=spawnSync('go',['build','-trimpath','-ldflags=-s -w','-o',output,'./cmd/quality-check'],{cwd:root,env:{...process.env,CGO_ENABLED:'0',GOOS:goos,GOARCH:goarch},stdio:'inherit'});
 if(result.status!==0)throw Error(`Build failed: ${name}`);
 await mkdir(path.join(root,'bin'),{recursive:true});await copyFile(output,path.join(root,'bin',name));
 const bytes=await readFile(output);manifest.artifacts.push({name,goos,goarch,size:bytes.length,sha256:createHash('sha256').update(bytes).digest('hex')});
 const bundle=path.join(root,'dist',`magpie-intelligence-modeltrace-${goos}-${goarch}`);await mkdir(path.join(bundle,'bin'),{recursive:true});
 for(const entry of ['plugin','assets','third_party','package.json','README.md','LICENSE'])await cp(path.join(root,entry),path.join(bundle,entry),{recursive:true});
 // Never distribute a runtime gate snapshot or its private probe alias.
 await writeFile(path.join(bundle,'plugin','gate.middleware.js'),'export function onRequest(body) { return body; }\n');
 await copyFile(output,path.join(bundle,'bin',goos==='windows'?'quality-check.exe':'quality-check'));
 const archive=path.basename(bundle)+'.tgz';
 const packed=spawnSync('tar',['-czf',path.join(root,'dist',archive),'-C',path.join(root,'dist'),path.basename(bundle)],{stdio:'inherit'});if(packed.status!==0)throw Error('Archive failed');
 const packedBytes=await readFile(path.join(root,'dist',archive));manifest.artifacts.push({name:archive,goos,goarch,size:packedBytes.length,sha256:createHash('sha256').update(packedBytes).digest('hex')});
}
await writeFile(path.join(root,'dist','manifest.json'),JSON.stringify(manifest,null,2)+'\n');
const local=platforms.find(([os,arch])=>os===({win32:'windows'}[process.platform]||process.platform)&&arch===({x64:'amd64',arm64:'arm64'}[process.arch]));
if(local){await mkdir(path.join(root,'bin'),{recursive:true});await copyFile(path.join(root,'dist',`quality-check-${local[0]}-${local[1]}${local[0]==='windows'?'.exe':''}`),path.join(root,'bin',local[0]==='windows'?'quality-check.exe':'quality-check'));}
console.log('Release binaries and SHA-256 manifest generated in dist/');
