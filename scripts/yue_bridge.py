"""Authenticated local YuE/Comfy bridge. No source changes or downloads.
Run with --project <YuE app> --state <private directory> --token-file <file>.
The bridge persists jobs and frozen YuE checkpoints. Recovery keeps the original
job ID; recreating a missing backend prompt requires an explicit choice.
"""
import sys
from pathlib import Path
sys.path.insert(0, str(Path(__file__).resolve().parent))
from process_window import no_console_options
import argparse, base64, concurrent.futures, copy, hashlib, hmac, json, math, os, re, secrets, threading, time, urllib.request, urllib.error, subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from singing_voice import validate_singing_config
from singing_config import load_singing_config
import yue_sampling
import rvc_cover
import rvc_recovery
import rvc_profiles
import yue_recovery
import station_rvc
import io
import tempfile
import wave
from urllib.parse import parse_qs, urlsplit
from music_account_scope import HEADER as ACCOUNT_HEADER, account_scope

def singing_profile(config_path,expected_sha256=None):
    if not config_path:return None
    path=Path(config_path)
    if expected_sha256 is None:
        data=path.read_bytes()
    else:
        with path.open('rb') as stream:data=stream.read(256*1024+1)
        if len(data)>256*1024 or hashlib.sha256(data).hexdigest()!=expected_sha256:
            raise ValueError('rvc_recovery_profile_changed')
    config=load_singing_config(path,data)
    if not config.get('enabled',False):return None
    if not all(isinstance(config.get(k),str) and config[k] for k in ('profile','python','runtime')):raise ValueError('singing_voice_config_invalid')
    if not Path(config['python']).is_file():raise ValueError('singing_voice_resources_missing')
    engine=validate_singing_config(config)
    return {'profile':config['profile'],'engine':engine,'config_sha256':hashlib.sha256(data).hexdigest(),'python':config['python']}

def requested_singing_profile(body,config_path):
    mode=body.get('singing_voice','native')
    if mode not in ('native','character'):raise ValueError('invalid_singing_voice')
    if mode=='native':return None
    voice=singing_profile(config_path)
    if voice is None:raise ValueError('singing_voice_not_ready')
    return voice

def convert_singing_voice(config_path,expected,source,destination,sandbox=None,*,cancelled=None):
    current=singing_profile(config_path,expected['config_sha256']) if cancelled is not None else singing_profile(config_path)
    if current is None or current['config_sha256']!=expected['config_sha256']:raise ValueError('singing_voice_configuration_changed')
    if cancelled is not None or not sandbox:
        converter=Path(__file__).with_name('singing_voice.py').resolve()
    else:
        converter=Path(sandbox['public'])/'projects/agent-tools/singing_voice.py'
    argv=[current['python'],str(converter),'--config',str(config_path),'--source',str(source),'--output',str(destination)]
    if cancelled is not None:
        argv.append('--offline')
    try:
        if sandbox:
            from managed_music_runner import run
            options={'cancelled':cancelled} if cancelled is not None else {}
            run(sandbox['public'],sandbox['private'],str(destination.parent),argv,gpu=True,timeout=3600,**options)
        else:subprocess.run(argv,check=True,timeout=3600,**no_console_options())
    except (subprocess.CalledProcessError,subprocess.TimeoutExpired):raise ValueError('singing_voice_conversion_failed') from None
    if not destination.is_file() or destination.stat().st_size<44:raise ValueError('singing_voice_output_missing')
    return destination

def apply_song_duration(graph,seconds):
    if isinstance(seconds,bool) or not isinstance(seconds,(int,float)) or not 10<=seconds<=600:raise ValueError('invalid_duration')
    found=False
    for node in graph.values():
        if node.get('class_type')=='YuE2GenerateMusic':
            node['inputs']['max_duration']=float(seconds);found=True
    if not found:raise ValueError('duration_control_missing')

def song_options(body):
    reference=body.get('reference_base64','')
    if not isinstance(reference,str):raise ValueError('invalid_reference')
    mode=body.get('mode') or ('cover' if reference else 'original')
    if mode not in ('original','cover'):raise ValueError('invalid_mode')
    if (mode=='cover')!=bool(reference):raise ValueError('cover_requires_reference')
    reference_mode=body.get('reference_mode','melody')
    if reference_mode not in ('melody','full'):raise ValueError('invalid_reference_mode')
    seed=body.get('seed')
    if seed is not None and (isinstance(seed,bool) or not isinstance(seed,int) or not 0<=seed<=2147483647):raise ValueError('invalid_seed')
    return mode,reference_mode,seed

FAITHFUL_COVER_STYLE='保持原曲调和原歌词 (preserve original melody and lyrics; follow the reference melody, rhythm and phrasing; no new melody or rewritten lyrics)'

def reference_window(body):
    start=body.get('reference_start_seconds',0)
    seconds=body.get('reference_seconds',body.get('duration_seconds',60))
    if isinstance(start,bool) or not isinstance(start,(int,float)) or not math.isfinite(start) or start<0:raise ValueError('invalid_reference_start')
    if isinstance(seconds,bool) or not isinstance(seconds,(int,float)) or not math.isfinite(seconds) or not 0<seconds<=600:raise ValueError('invalid_reference_seconds')
    return start,seconds

def generation_style(body):
    """Same effective conditioning for persisted recipe and submitted graph."""
    mode,_,_=song_options(body)
    gender=body.get('vocal_gender') or 'female'
    if gender not in ('female','male'):raise ValueError('invalid_vocal_gender')
    style=re.sub(r'\b(?:fe)?male\s+(?:vocals?|singer|voice)\b|男声|女声','',body['style'],flags=re.I).strip(' ,;，；\n\r\t')
    if mode=='cover' and FAITHFUL_COVER_STYLE not in style:style+=', '+FAITHFUL_COVER_STYLE
    # Put the requested singer first as well as last. A source artist's vocal
    # register is not a request to imitate that artist's sex or timbre.
    lead='solo female lead singer, clear expressive female singing voice' if gender=='female' else 'solo male lead singer, expressive male singing voice'
    return lead+', '+style+', '+gender+' vocal'


def generation_controls(body):
    """Native guidance and an acceptance range, not a promise about output."""
    mode,_,_=song_options(body)
    guidance=body.get('text_guidance_scale',2.0 if mode=='cover' else 1.0)
    minimum=body.get('min_duration_seconds',body.get('duration_seconds',60)*.9)
    for value in (guidance,minimum):
        if isinstance(value,bool) or not isinstance(value,(int,float)) or not math.isfinite(value):raise ValueError('invalid_generation_controls')
    if not 0<=guidance<=100 or not 0<=minimum<=body.get('duration_seconds',60):raise ValueError('invalid_generation_controls')
    return guidance,minimum


def duration_quality(actual,body):
    _,minimum=generation_controls(body)
    target=body.get('duration_seconds',60)
    return {'requested_seconds':target,'actual_seconds':actual,'minimum_seconds':minimum,
            'duration_passed':math.isfinite(actual) and actual>0 and minimum<=actual<=target+.5,
            'vocal_gender_requested':body.get('vocal_gender') or 'female',
            'vocal_gender_verified':False}

def song_recipe(body):
    """Persist the submitted recipe privately, never infer it from filenames."""
    mode,reference_mode,seed=song_options(body)
    result={k:body.get(k) for k in ('title','style','lyrics','duration_seconds','singing_voice','vocal_gender')}
    result.update(style=generation_style(body),vocal_gender=body.get('vocal_gender') or 'female',singing_voice=body.get('singing_voice') or 'native')
    result.update(mode=mode,reference_mode=reference_mode,seed=seed)
    guidance,minimum=generation_controls(body)
    result.update(text_guidance_scale=guidance,min_duration_seconds=minimum)
    result.update({k:body[k] for k in ('sampling','planning_mode') if k in body})
    if mode=='cover':
        try:data=base64.b64decode(body['reference_base64'],validate=True)
        except Exception:raise ValueError('invalid_reference_encoding') from None
        if not 0<len(data)<=25*1024*1024:raise ValueError('reference_size_limit')
        result['reference_sha256']=hashlib.sha256(data).hexdigest()
        start,seconds=reference_window(body)
        result.update(reference_start_seconds=start,reference_seconds=seconds)
        if 'reference_pretrimmed' in body and not isinstance(body['reference_pretrimmed'],bool):raise ValueError('invalid_reference_pretrimmed')
        result.update(reference_pretrimmed=body.get('reference_pretrimmed',False),reference_source_sha256=body.get('reference_source_sha256'))
    return result

def prepare_song_graph(workflow_dir,body,jid,input_dir,template=None):
    mode,reference_mode,seed=song_options(body)
    if seed is None:raise ValueError('seed_not_persisted')
    graph=copy.deepcopy(template) if template is not None else json.loads((workflow_dir/('audio_yue2_music_cover.json' if mode=='cover' else 'audio_yue2_text_to_music.json')).read_text(encoding='utf-8'))
    target=body.get('duration_seconds',60);apply_song_duration(graph,target)
    graph['33:25']['inputs']['cfg_scale']=generation_controls(body)[0]
    yue_sampling.apply(graph,body)
    style=generation_style(body)
    # Preserve the user's supplied style/lyrics. Duration is controlled by the
    # real generation node, not by silently injecting a new short-song style.
    if mode=='cover':
        try:data=base64.b64decode(body['reference_base64'],validate=True)
        except Exception:raise ValueError('invalid_reference_encoding') from None
        if not 0<len(data)<=25*1024*1024:raise ValueError('reference_size_limit')
        input_dir.mkdir(parents=True,exist_ok=True)
        source=input_dir/('wechat-ref-'+jid+'.audio');source.write_bytes(data)
        name='wechat-ref-'+jid+'.wav'
        start,seconds=reference_window(body)
        seek=0 if body.get('reference_pretrimmed',False) else start
        subprocess.run(['ffmpeg','-v','error','-nostdin','-y','-protocol_whitelist','file','-i',str(source),'-ss',str(seek),'-vn','-ar','44100','-ac','2','-t',str(seconds),str(input_dir/name)],check=True,timeout=90,**no_console_options())
        graph['45']['inputs']['audio']=name
        graph['33:25']['inputs']['style']=style;graph['33:25']['inputs']['lyrics']=body['lyrics']
        graph['33:25']['inputs']['mode']=reference_mode
        graph['33:41']['inputs']['mode']=reference_mode
    else:
        graph['33:36']['inputs']['value']=style;graph['33:37']['inputs']['value']=body['lyrics']
    graph['33:34']['inputs']['seed']=seed
    graph['10']['inputs']['filename_prefix']='wechat-agent/'+jid
    return graph

def station_ready_file(value,nonce,token_path,scope):
    from managed_music_runner import canonical
    if not re.fullmatch(r'[a-f0-9]{64}',nonce or ''):
        raise ValueError('station_rvc_ready_nonce_invalid')
    path=Path(value)
    if not path.is_absolute() or path.name!=scope+'-'+nonce+'.json':
        raise ValueError('station_rvc_ready_path_invalid')
    parent=canonical(path.parent)
    if parent!=token_path.parent/'station-rvc-ready':
        raise ValueError('station_rvc_ready_path_invalid')
    return parent/path.name

def signal_station_ready(path,nonce,port,scope):
    fd,temporary=tempfile.mkstemp(dir=path.parent,prefix='.station-rvc-ready-')
    try:
        with os.fdopen(fd,'w',encoding='utf-8') as file:
            json.dump({'nonce':nonce,'port':port,'scope':scope,'pid':os.getpid()},file)
            file.flush()
            os.fsync(file.fileno())
        os.link(temporary,path)
    finally:
        Path(temporary).unlink(missing_ok=True)

def main():
    p=argparse.ArgumentParser();p.add_argument('--project',required=True);p.add_argument('--workflow-dir');p.add_argument('--state',required=True);p.add_argument('--token-file',required=True);p.add_argument('--port',type=int,default=6188);p.add_argument('--bind',default='127.0.0.1');p.add_argument('--singing-voice-config');p.add_argument('--managed-public');p.add_argument('--managed-private');p.add_argument('--comfy-socket');p.add_argument('--comfy-input')
    p.add_argument('--rvc-only',action='store_true')
    p.add_argument('--station-fish')
    p.add_argument('--desktop',action='store_true')
    p.add_argument('--ready-file')
    p.add_argument('--ready-nonce')
    args=p.parse_args()
    station=args.rvc_only
    if (station != bool(args.station_fish) or args.desktop and not station or
            bool(args.ready_file) != bool(args.ready_nonce) or args.ready_file and not station):
        p.error('station_rvc_scope_required')
    project=Path(args.project).resolve()
    workflow_dir=Path(args.workflow_dir).resolve() if args.workflow_dir else project/'workflows_api'
    sandbox=None;bound_account='';scope_header=ACCOUNT_HEADER
    token_path=Path(args.token_file)
    stopping=threading.Event()
    ready_path=None
    if station:
        if args.bind!='127.0.0.1' or args.workflow_dir or args.comfy_socket or args.comfy_input or args.singing_voice_config:
            p.error('station_rvc_requires_isolated_loopback_and_explicit_profiles')
        public,private,state,token_path,bound_account=station_rvc.bind_scope(args.station_fish,args.managed_public,args.managed_private,args.state,args.token_file)
        if token_path.is_relative_to(Path(__file__).resolve().parent):
            p.error('station_rvc_token_inside_control_package')
        if args.ready_file:
            ready_path=station_ready_file(args.ready_file,args.ready_nonce,token_path,bound_account)
        sandbox={'public':str(public),'private':str(private)}
        scope_header=station_rvc.HEADER
        input_dir=None
    else:
        state=Path(args.state).resolve();state.mkdir(parents=True,exist_ok=True)
        if args.managed_public or args.managed_private:
            if not args.managed_public or not args.managed_private:raise ValueError('music_managed_scope_incomplete')
            from managed_music_runner import canonical
            sandbox={'public':str(canonical(args.managed_public)),'private':str(canonical(args.managed_private))}
            bound_account=account_scope(sandbox['private'])
            if not state.is_relative_to(Path(sandbox['private'])):raise ValueError('music_state_outside_account')
            if not args.comfy_socket or not args.comfy_input:raise ValueError('isolated_comfy_paths_required')
        input_dir=Path(args.comfy_input).resolve() if args.comfy_input else project/'comfy/ComfyUI/input'
        if sandbox and not input_dir.is_relative_to(Path(sandbox['private'])):raise ValueError('music_input_outside_account')
        input_dir.mkdir(parents=True,exist_ok=True)
        for filename in ('audio_yue2_music_cover.json','audio_yue2_text_to_music.json'):
            if not (workflow_dir/filename).is_file():raise ValueError('workflow_missing: '+filename)

    def comfy_open(request,timeout=20):
        if args.comfy_socket:
            from music_unix_client import open_request
            return open_request(args.comfy_socket,request,timeout)
        return urllib.request.urlopen(request,timeout=timeout)

    def selected_rvc_profile(body):
        if station:
            return station_rvc.checked_profile(public,private,body,singing_profile)
        config=rvc_profiles.resolve(args.managed_public,args.singing_voice_config,body,args.managed_private)
        return config,singing_profile(config)

    token=token_path.read_text().strip();lock=threading.RLock();pool=concurrent.futures.ThreadPoolExecutor(max_workers=1)
    if station and not token:
        raise ValueError('station_rvc_authentication_required')
    state_lease=rvc_recovery.lock_state(state)
    db=state/'jobs.json'
    if station and (db.is_symlink() or getattr(db,'is_junction',lambda:False)() or db.exists() and not db.is_file()):
        raise ValueError('station_rvc_journal_invalid')
    jobs=json.loads(db.read_bytes()) if db.exists() else {}
    def save():
        rvc_recovery.save_jobs(db,jobs)
    def persist_job(jid, changes):
        previous=jobs.get(jid)
        jobs[jid]={**(previous or {}),**changes}
        try:
            save()
        except OSError:
            if previous is None:
                jobs.pop(jid,None)
            else:
                jobs[jid]=previous
            raise
    if station and any(job.get('engine')!='rvc' or job.get('station_fish')!=bound_account for job in jobs.values()):
        raise ValueError('station_rvc_journal_scope_mismatch')
    interrupted_yue=[]
    for job in jobs.values():
        if station and job['state']=='cancelling':
            job.update(state='cancelled',phase='cancelled',reason='station_rvc_process_stopped')
            continue
        if job['state'] in ('queued','running') and job.get('engine')!='rvc' and (job.get('checkpoint_sha256') or job.get('queued_checkpoint_sha256')):
            interrupted_yue.append(job['id'])
        if job['state'] in ('queued','running'):
            job.update(state='needs_review',reason='bridge_restarted_no_replay')
            if station:job['phase']='needs_review'
    save()
    def api(route,body=None,timeout=20):
        req=urllib.request.Request('http://127.0.0.1:8188'+route,data=None if body is None else json.dumps(body).encode(),headers={'Content-Type':'application/json'})
        with comfy_open(req,timeout=timeout) as r:
            data=r.read()
            return json.loads(data) if data else {}
    def run(jid,body,checkpoint=None,existing_prompt=None):
        try:
            with lock:
                if jobs[jid]['state']=='cancelled':return
                jobs[jid]['state']='running';save()
            target=body.get('duration_seconds',60)
            prepared=bool(checkpoint and checkpoint.get('prepared',True))
            graph=checkpoint['graph'] if prepared else prepare_song_graph(workflow_dir,body,jid,input_dir,template=checkpoint['graph'] if checkpoint else None)
            if not prepared and song_options(body)[0]=='cover':
                prepared=input_dir/('wechat-ref-'+jid+'.wav')
                seconds=float(subprocess.check_output(**no_console_options(), args=['ffprobe','-v','error','-show_entries','format=duration','-of','default=noprint_wrappers=1:nokey=1',str(prepared)],timeout=15))
                start,requested=reference_window(body)
                if seconds+.1<requested:raise ValueError('reference_window_incomplete')
                with lock:jobs[jid]['reference_window']={'start_seconds':start,'requested_seconds':requested,'prepared_seconds':seconds,'prepared_sha256':hashlib.sha256(prepared.read_bytes()).hexdigest()};save()
            if existing_prompt:
                prompt_id=existing_prompt
            else:
                with lock:
                    if not prepared:
                        jobs[jid]['checkpoint_sha256']=yue_recovery.persist(state,jid,body,graph,input_dir)
                        jobs[jid]['graph_sha256']=yue_recovery.digest(graph)
                    jobs[jid].update(phase='submitting_yue',submission_attempts=jobs[jid].get('submission_attempts',0)+1)
                    save()
                submitted=api('/prompt',{'prompt':graph,'client_id':'wechat-yue-'+jid,
                                         'extra_data':{'wechat_graph_sha256':jobs[jid]['graph_sha256']}})
                prompt_id=submitted['prompt_id']
            with lock:jobs[jid].update(prompt_id=prompt_id,phase='waiting_yue');save()
            for _ in range(10800):
                history=api('/history/'+prompt_id).get(prompt_id)
                if history:
                    if history.get('status',{}).get('status_str')=='error':
                        with lock:jobs[jid]['backend_error']=yue_recovery.backend_failure(history);save()
                        raise ValueError('comfy_execution_error')
                    files=[item for node in history.get('outputs',{}).values() for key,items in node.items() if isinstance(items,list) for item in items if isinstance(item,dict) and item.get('filename') and item.get('type')=='output']
                    audio=next((x for x in files if Path(x['filename']).suffix.lower() in ('.wav','.mp3','.flac','.ogg','.m4a')),None)
                    if not audio:raise ValueError('no_generated_audio')
                    from urllib.parse import urlencode
                    with comfy_open('http://127.0.0.1:8188/view?'+urlencode(audio),timeout=60) as r:data=r.read(64*1024*1024+1)
                    if not 0<len(data)<=64*1024*1024:raise ValueError('output_size_limit')
                    output=state/(jid+Path(audio['filename']).suffix);output.write_bytes(data)
                    duration=float(subprocess.check_output(**no_console_options(), args=['ffprobe','-v','error','-show_entries','format=duration','-of','default=nw=1:nk=1',str(output)],timeout=30))
                    if not math.isfinite(duration) or duration<=0:raise ValueError('generation_audio_invalid')
                    quality=duration_quality(duration,body)
                    with lock:jobs[jid].update(duration_seconds=duration,requested_seconds=target,quality=quality,file=output.name,bytes=output.stat().st_size);save()
                    if duration<quality['minimum_seconds']:raise ValueError('generation_duration_shortfall')
                    if duration>target+0.5:raise ValueError('generation_duration_exceeded')
                    # Remove only codec padding, never shorten a multi-minute result
                    # and pretend the generator followed the requested duration.
                    if duration>target:
                        adjusted=state/(jid+'-bounded.flac')
                        subprocess.run(['ffmpeg','-v','error','-y','-i',str(output),'-t',str(target),'-c:a','flac',str(adjusted)],check=True,timeout=60,**no_console_options())
                        output=adjusted;duration=float(target)
                    voice=jobs[jid].get('singing_voice')
                    if voice:
                        with lock:jobs[jid]['phase']='converting_voice';save()
                        api('/free',{'unload_models':True,'free_memory':True})
                        output=convert_singing_voice(args.singing_voice_config,voice,output,state/(jid+'-character-voice.wav'),sandbox=sandbox)
                    with lock:jobs[jid].update(state='completed',phase='completed',file=output.name,bytes=output.stat().st_size,duration_seconds=duration,requested_seconds=target);save()
                    return
                time.sleep(1)
            raise ValueError('generation_deadline')
        except Exception as e:
            with lock:
                uncertain=not isinstance(e,ValueError) and jobs[jid].get('phase') in ('submitting_yue','waiting_yue')
                jobs[jid].update(state='needs_review' if uncertain else 'failed',reason=str(e)[:120] if isinstance(e,ValueError) else type(e).__name__)
                save()
        finally:
            with lock:idle=not any(j['state'] in ('queued','running') for j in jobs.values())
            if idle:
                try:
                    queue=api('/queue',timeout=5)
                    if not queue.get('queue_running') and not queue.get('queue_pending'):
                        api('/free',{'unload_models':True,'free_memory':True})
                except Exception:pass
    def run_rvc(jid,body):
        def cancelled():
            with lock:
                return stopping.is_set() or jobs[jid]['state'] in ('cancelling','cancelled')
        try:
            with lock:
                if jobs[jid]['state']=='cancelled':return
                if station:
                    persist_job(jid,{'state':'running','phase':'converting'})
                else:
                    jobs[jid]['state']='running';save()
                profile=dict(jobs[jid]['singing_voice'])
            if not station:
                # The legacy combined broker can reclaim idle Comfy GPU models.
                try:
                    queue=api('/queue',timeout=5)
                    if not queue.get('queue_running') and not queue.get('queue_pending'):
                        api('/free',{'unload_models':True,'free_memory':True},timeout=5)
                except Exception:pass
            config,current=selected_rvc_profile(body)
            if not current or current.get('config_sha256')!=profile.get('config_sha256'):
                raise ValueError('rvc_recovery_profile_changed')

            def convert_voice(config,expected,source,destination):
                if station:
                    if cancelled():raise ValueError('station_rvc_interrupted')
                    convert_singing_voice(config,expected,source,destination,sandbox=sandbox,cancelled=cancelled)
                else:
                    convert_singing_voice(config,expected,source,destination,sandbox=sandbox)

            output=rvc_cover.convert(body,state,jid,config,profile,convert_voice)
            if station:
                if cancelled():raise ValueError('station_rvc_interrupted')
                output=station_rvc.verify_output(state,jid,output,profile)
            with lock:
                if station and cancelled():raise ValueError('station_rvc_interrupted')
                if station:
                    persist_job(jid,{**output,'state':'completed','phase':'completed'})
                else:
                    jobs[jid].update(output,state='completed',phase='completed');save()
        except Exception as error:
            with lock:
                if station and jobs[jid]['state']=='cancelling':
                    persist_job(jid,{'state':'cancelled','phase':'cancelled','reason':'station_rvc_cancelled'})
                elif station and stopping.is_set():
                    persist_job(jid,{'state':'needs_review','phase':'needs_review','reason':'station_rvc_stopped_no_replay'})
                else:
                    reason=str(error)[:120] if isinstance(error,ValueError) else type(error).__name__
                    if station:
                        if not re.fullmatch(r'[A-Za-z0-9_]{1,120}',reason):
                            reason='station_rvc_conversion_failed'
                        persist_job(jid,{'state':'failed','phase':'failed','reason':reason})
                    else:
                        jobs[jid].update(state='failed',reason=reason);save()
    def resume_yue(jid,restart_missing=False,restart_failed=False):
        return yue_recovery.resume(jobs,jid,state,input_dir,song_recipe,
                                   lambda:singing_profile(args.singing_voice_config),api,save,
                                   lambda ident,checkpoint,prompt:pool.submit(run,ident,checkpoint['body'],checkpoint,prompt),
                                   restart_missing=restart_missing,restart_failed=restart_failed)
    # Only reconnect to an existing prompt on startup. A lost backend requires
    # an explicit restart choice, never an unattended second generation.
    for jid in interrupted_yue:
        try:
            with lock:resume_yue(jid)
        except Exception as error:
            with lock:
                jobs[jid].update(state='needs_review',reason=str(error)[:120] if isinstance(error,ValueError) else type(error).__name__)
                save()
    class Handler(BaseHTTPRequestHandler):
        def log_message(self,*args):pass
        def bound(self):
            return self.authenticated() and hmac.compare_digest(self.headers.get(scope_header,''),bound_account)
        def job_view(self,job):
            hidden={'file'}
            if station:hidden.update(('key','request_hash','station_fish'))
            return {k:v for k,v in job.items() if k not in hidden}
        def reply(self,status,value):
            data=json.dumps(value,ensure_ascii=False).encode();self.send_response(status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(data)))
            if bound_account and (self.bound() if station else self.authenticated()):self.send_header(scope_header,bound_account)
            self.end_headers();self.wfile.write(data)
        def authenticated(self):return hmac.compare_digest(self.headers.get('Authorization',''),'Bearer '+token)
        def allowed(self):
            if not self.authenticated():self.reply(401,{'error':'unauthorized'});return False
            if bound_account and not self.bound():
                self.reply(409,{'error':'station_rvc_scope_mismatch' if station else 'music_account_scope_mismatch'});return False
            return True
        def do_GET(self):
            if not self.allowed():return
            if station:return self.get_station_rvc()
            if self.path=='/rvc-health':
                try:
                    voice=singing_profile(args.singing_voice_config)
                    return self.reply(200,{'ready':bool(voice and voice['engine']=='rvc'),'bridge_version':13,'engine':'rvc','voice_profiles':True,'private_voice_profiles':True,'profile':voice['profile'] if voice else None,'resume_same_job':True})
                except Exception:return self.reply(200,{'ready':False,'bridge_version':13,'engine':'rvc','voice_profiles':True,'private_voice_profiles':True,'resume_same_job':True})
            if self.path=='/parameters':
                try:
                    info={kind:api('/object_info/'+kind)[kind] for _,kind,_ in yue_sampling.GROUPS.values()}
                    return self.reply(200,{'bridge_version':13,'modes':{mode:yue_sampling.options(workflow_dir,mode,info) for mode in ('original','cover')}})
                except Exception:return self.reply(503,{'error':'sampling_schema_unavailable'})
            if self.path=='/health':
                try:
                    api('/system_stats')
                    try:voice=singing_profile(args.singing_voice_config)
                    except Exception:voice=None
                    return self.reply(200,{'ready':True,'bridge_version':13,'generation_policy':'female-faithful-cover-v4-verified-duration','capabilities':['original','reference_cover','reference_window','text_guidance_scale','duration_quality','seed','reference_mode','sampling','planning_mode','direct_rvc_cover','rvc_resume_same_job','yue_resume_same_job','private_voice_profiles']+(['singing_voice_conversion'] if voice else []),'singing_voice_profile':voice['profile'] if voice else None,'cover_boundary':'ABC-conditioned resynthesis; optional local singing conversion after generation; requested gender is not listening verification; max duration is a ceiling'})
                except Exception:return self.reply(200,{'ready':False})
            parts=self.path.strip('/').split('/');jid=parts[1] if len(parts)>1 else ''
            with lock:job=dict(jobs.get(jid,{}))
            if not job:return self.reply(404,{'error':'not_found'})
            if len(parts)==3 and parts[2]=='audio':
                if job['state']!='completed':return self.reply(409,{'error':'not_completed'})
                file=state/job['file'];data=file.read_bytes();self.send_response(200);self.send_header('Content-Type','audio/flac' if file.suffix=='.flac' else 'audio/wav');self.send_header('Content-Length',str(len(data)))
                if bound_account:self.send_header(ACCOUNT_HEADER,bound_account)
                self.end_headers();self.wfile.write(data);return
            job.pop('file',None);return self.reply(200,job)
        def get_station_rvc(self):
            route=urlsplit(self.path)
            if route.path=='/health' and not route.query:
                return self.reply(200,{'ready':True,'engine':'rvc','mode':'station_rvc','bridge_version':1})
            if route.path=='/rvc-health':
                args=parse_qs(route.query,keep_blank_values=True)
                values=args.get('voice_profile_id',[])
                if set(args)!={'voice_profile_id'} or len(values)!=1 or not values[0]:
                    return self.reply(400,{'ready':False,'error':'station_rvc_profile_required'})
                try:
                    _,profile=selected_rvc_profile({'voice_profile_id':values[0]})
                except Exception:
                    return self.reply(409,{'ready':False,'error':'station_rvc_profile_unavailable'})
                return self.reply(200,{'ready':True,'engine':'rvc','voice_profile_id':values[0],**profile})
            if route.query:
                return self.reply(404,{'error':'not_found'})
            lookup=re.fullmatch(r'/rvc-requests/([a-f0-9]{64})',route.path)
            if lookup:
                with lock:job=next((dict(j) for j in jobs.values() if j.get('key')==lookup[1]),None)
                if job is None:return self.reply(404,{'error':'not_found'})
                return self.reply(200,self.job_view(job))
            match=re.fullmatch(r'/jobs/([a-f0-9]{32})(/audio)?',route.path)
            if not match:return self.reply(404,{'error':'not_found'})
            jid=match[1]
            with lock:job=dict(jobs.get(jid,{}))
            if not job:return self.reply(404,{'error':'not_found'})
            if not match[2]:return self.reply(200,self.job_view(job))
            if job['state']!='completed':return self.reply(409,{'error':'not_completed'})
            try:data=station_rvc.read_output(state,jid,job)
            except (OSError,ValueError):
                return self.reply(409,{'error':'station_rvc_artifact_unavailable'})
            self.send_response(200)
            self.send_header('Content-Type','audio/wav')
            self.send_header('Content-Length',str(len(data)))
            self.send_header(scope_header,bound_account)
            self.end_headers()
            self.wfile.write(data)
        def do_POST(self):
            if not self.allowed():return
            try:size=int(self.headers.get('Content-Length','0'))
            except ValueError:return self.reply(400,{'error':'invalid_content_length'})
            if size<=0 or size>36*1024*1024:return self.reply(413,{'error':'size_limit'})
            try:body=json.loads(self.rfile.read(size))
            except Exception:return self.reply(400,{'error':'invalid_json'})
            if not isinstance(body,dict):return self.reply(400,{'error':'invalid_json'})
            if station:
                if stopping.is_set():return self.reply(503,{'error':'station_rvc_stopping'})
                cancel=re.fullmatch(r'/jobs/([a-f0-9]{32})/cancel',self.path)
                if cancel:
                    if body:return self.reply(400,{'error':'station_rvc_cancel_no_overrides'})
                    return self.cancel_station_rvc(cancel[1])
                resume=re.fullmatch(r'/jobs/([a-f0-9]{32})/resume',self.path)
                if resume:
                    if body:return self.reply(400,{'error':'rvc_recovery_no_recipe_overrides'})
                    return self.resume_station_rvc(resume[1])
                if self.path=='/rvc-jobs':return self.submit_rvc(body)
                return self.reply(404,{'error':'not_found'})
            if re.fullmatch(r'/jobs/[a-f0-9]{32}/resume',self.path):
                jid=self.path.split('/')[2]
                try:
                    with lock:
                        if jobs.get(jid,{}).get('engine')=='rvc':
                            if body:return self.reply(400,{'error':'rvc_recovery_no_recipe_overrides'})
                            config=rvc_profiles.resolve(args.managed_public,args.singing_voice_config,jobs[jid].get('recipe',{}),args.managed_private)
                            job=rvc_recovery.resume(jobs,jid,state,config,singing_profile,save,
                                                   lambda job_id,request:pool.submit(run_rvc,job_id,request))
                        else:
                            if set(body)-{'restart_missing','restart_failed'} or any(not isinstance(body.get(key,False),bool) for key in ('restart_missing','restart_failed')):
                                return self.reply(400,{'error':'yue_recovery_no_recipe_overrides'})
                            job=resume_yue(jid,body.get('restart_missing',False),body.get('restart_failed',False))
                    return self.reply(200,self.job_view(job))
                except ValueError as error:return self.reply(409,{'error':str(error)})
                except Exception:return self.reply(503,{'error':'music_recovery_unavailable'})
            if self.path=='/rvc-jobs':return self.submit_rvc(body)
            if self.path!='/jobs':return self.reply(404,{'error':'not_found'})
            if not isinstance(body.get('style'),str) or not isinstance(body.get('lyrics'),str) or len(body['style'])>2000 or len(body['lyrics'])>6000:return self.reply(400,{'error':'invalid_song_input'})
            target=body.get('duration_seconds',60)
            if isinstance(target,bool) or not isinstance(target,(int,float)) or not 10<=target<=600:return self.reply(400,{'error':'invalid_duration'})
            try:
                mode,reference_mode,seed=song_options(body)
                recipe=song_recipe(body)
            except ValueError as e:return self.reply(400,{'error':str(e)})
            try:voice=requested_singing_profile(body,args.singing_voice_config)
            except ValueError as e:return self.reply(400,{'error':str(e)})
            except Exception:return self.reply(503,{'error':'singing_voice_not_ready'})
            # Keep a request fingerprint before assigning an optional seed. A
            # reused key cannot silently change the reference, lyrics or take.
            fingerprint=hashlib.sha256(json.dumps(body,sort_keys=True,ensure_ascii=False).encode()).hexdigest()
            key=self.headers.get('Idempotency-Key','')
            # Resolve an existing idempotent result even while the backend is
            # offline. A new job requires a live generator, before persistence.
            with lock:
                for old in jobs.values():
                    if key and old.get('key')==key:
                        if old.get('request_hash') and old['request_hash']!=fingerprint:return self.reply(409,{'error':'idempotency_conflict'})
                        return self.reply(200,{k:v for k,v in old.items() if k!='file'})
            try:
                api('/system_stats',timeout=5)
                info={kind:api('/object_info/'+kind)[kind] for _,kind,_ in yue_sampling.GROUPS.values()}
            except Exception:return self.reply(503,{'error':'backend_unavailable'})
            try:
                effective=yue_sampling.resolve(body,workflow_dir,info)
            except ValueError as e:return self.reply(400,{'error':str(e)})
            body.update(effective);recipe.update(effective)
            with lock:
                for old in jobs.values():
                    if key and old.get('key')==key:
                        if old.get('request_hash') and old['request_hash']!=fingerprint:return self.reply(409,{'error':'idempotency_conflict'})
                        return self.reply(200,{k:v for k,v in old.items() if k!='file'})
                if len(jobs)>=500:return self.reply(409,{'error':'job_history_limit'})
                if sum(j['state'] in ('queued','running') for j in jobs.values())>=3:return self.reply(429,{'error':'queue_full'})
                if seed is None:seed=secrets.randbelow(2**31)
                body['seed']=seed
                recipe['seed']=seed
                jid=secrets.token_hex(16)
                template=yue_sampling.load_graph(workflow_dir,mode)
                try:queued_hash=yue_recovery.persist(state,jid,body,template,input_dir,prepared=False)
                except Exception:return self.reply(503,{'error':'yue_request_persist_failed'})
                jobs[jid]={'id':jid,'engine':'yue','state':'queued','key':key,'created':time.time(),'reference_used':mode=='cover','mode':mode,'reference_mode':reference_mode,'seed':seed,'request_hash':fingerprint,'requested_seconds':target,'recipe':recipe,'queued_checkpoint_sha256':queued_hash,'queued_graph_sha256':yue_recovery.digest(template),'singing_voice':{k:v for k,v in voice.items() if k!='python'} if voice else None}
                save();pool.submit(run,jid,body,{'body':body,'graph':template,'prepared':False})
            self.reply(202,{'effective_parameters':effective,'id':jid,'state':'queued','seed':seed,'mode':mode,'reference_used':mode=='cover','singing_voice_profile':voice['profile'] if voice else None})
        def cancel_station_rvc(self,jid):
            try:
                with lock:
                    job=jobs.get(jid)
                    if job is None:return self.reply(404,{'error':'not_found'})
                    if job['state'] in ('queued','needs_review','failed'):
                        persist_job(jid,{'state':'cancelled','phase':'cancelled','reason':'station_rvc_cancelled'})
                    elif job['state']=='running':
                        persist_job(jid,{'state':'cancelling','phase':'cancelling'})
                    view=self.job_view(jobs[jid])
            except OSError:
                return self.reply(503,{'error':'station_rvc_cancel_persist_failed'})
            return self.reply(200,view)
        def resume_station_rvc(self,jid):
            try:
                with lock:
                    job=jobs.get(jid)
                    if job is None:return self.reply(404,{'error':'not_found'})
                    if job['state'] in ('queued','running','completed'):
                        return self.reply(200,self.job_view(job))
                    if job['state'] not in ('failed','needs_review'):
                        return self.reply(409,{'error':'rvc_recovery_state_not_resumable'})
                    recipe=job['recipe']
                config,profile=selected_rvc_profile(recipe)
                with lock:
                    if stopping.is_set():return self.reply(503,{'error':'station_rvc_stopping'})
                    result=rvc_recovery.resume(jobs,jid,state,config,lambda _:profile,save,
                                               lambda job_id,request:pool.submit(run_rvc,job_id,request))
            except ValueError as error:
                code=str(error)
                return self.reply(409,{'error':code if re.fullmatch(r'[a-z0-9_]+',code) else 'station_rvc_recovery_unavailable'})
            except OSError:
                return self.reply(503,{'error':'station_rvc_recovery_unavailable'})
            return self.reply(200,self.job_view(result))
        def rvc_duplicate(self,key,fingerprint):
            for old in jobs.values():
                if key and old.get('key')==key:
                    if old.get('request_hash')!=fingerprint:return 409,{'error':'idempotency_conflict'}
                    return 200,self.job_view(old)
            return None
        def submit_rvc(self,body):
            try:details=rvc_cover.recipe(body)
            except ValueError as error:return self.reply(400,{'error':str(error)})
            fingerprint=hashlib.sha256(json.dumps(body,sort_keys=True,ensure_ascii=False).encode()).hexdigest()
            key=self.headers.get('Idempotency-Key','')
            if station:
                if not re.fullmatch(r'[a-f0-9]{64}',key):
                    return self.reply(400,{'error':'station_rvc_request_key_required'})
                if not body.get('voice_profile_id'):
                    return self.reply(400,{'error':'station_rvc_profile_required'})
                pin=body.get('profile_config_sha256')
                if not isinstance(pin,str) or not re.fullmatch(r'[a-f0-9]{64}',pin):
                    return self.reply(400,{'error':'station_rvc_profile_pin_required'})
                if not isinstance(details['reference_source_sha256'],str) or not re.fullmatch(r'[a-f0-9]{64}',details['reference_source_sha256']):
                    return self.reply(400,{'error':'station_rvc_source_hash_required'})
                if set(body)-{'title','reference_base64','reference_start_seconds','reference_seconds','reference_pretrimmed','reference_source_sha256','voice_profile_id','profile_config_sha256'}:
                    return self.reply(400,{'error':'station_rvc_unknown_request_field'})
                try:
                    with wave.open(io.BytesIO(base64.b64decode(body['reference_base64'],validate=True)),'rb') as wav:
                        frames=wav.getnframes()
                        if (wav.getsampwidth()!=2 or wav.getnchannels() not in (1,2) or wav.getframerate()<1 or frames<1 or
                                abs(frames/wav.getframerate()-details['reference_seconds'])>.25 or
                                len(wav.readframes(frames))!=frames*wav.getnchannels()*2):
                            return self.reply(400,{'error':'station_rvc_pcm16_reference_required'})
                except (EOFError,ValueError,wave.Error):
                    return self.reply(400,{'error':'station_rvc_pcm16_reference_required'})
            with lock:duplicate=self.rvc_duplicate(key,fingerprint)
            if duplicate:return self.reply(*duplicate)
            try:_,voice=selected_rvc_profile(body)
            except Exception:return self.reply(503,{'error':'rvc_profile_unavailable'})
            if not voice or voice['engine']!='rvc':return self.reply(503,{'error':'rvc_profile_required'})
            if station and voice.get('config_sha256')!=pin:
                return self.reply(409,{'error':'station_rvc_profile_changed'})
            with lock:
                duplicate=self.rvc_duplicate(key,fingerprint)
                if duplicate:return self.reply(*duplicate)
                if station and len(jobs)>=500:return self.reply(409,{'error':'job_history_limit'})
                if station and sum(j['state'] in ('queued','running','cancelling') for j in jobs.values())>=3:
                    return self.reply(429,{'error':'queue_full'})
                if station and stopping.is_set():return self.reply(503,{'error':'station_rvc_stopping'})
                jid=secrets.token_hex(16)
                # Persist the verified source BEFORE the durable queued job.
                # Restart recovery must not depend on a lost in-memory body.
                try:rvc_recovery.persist_reference(state,jid,body)
                except Exception:return self.reply(503,{'error':'rvc_reference_persist_failed'})
                job={'id':jid,'state':'queued','key':key,'created':time.time(),'engine':'rvc','mode':'cover','reference_used':True,'request_hash':fingerprint,
                           'requested_seconds':details['reference_seconds'],'recipe':details,'singing_voice':{k:v for k,v in voice.items() if k!='python'}}
                if station:job['station_fish']=bound_account
                try:persist_job(jid,job)
                except OSError:return self.reply(503,{'error':'rvc_job_persist_failed'})
                try:pool.submit(run_rvc,jid,body)
                except RuntimeError:
                    if not station:raise
                    persist_job(jid,{'state':'needs_review','phase':'needs_review','reason':'station_rvc_schedule_unavailable'})
                    return self.reply(503,{'error':'station_rvc_schedule_unavailable','id':jid})
            return self.reply(202,{'id':jid,'state':'queued','engine':'rvc','reference_used':True,'singing_voice_profile':voice['profile'],'voice_profile_id':body.get('voice_profile_id','')})
    # Windows/WSL access: token required on every endpoint. No remote code/API.
    server=ThreadingHTTPServer((args.bind,args.port),Handler)
    try:
        if ready_path:
            signal_station_ready(ready_path,args.ready_nonce,server.server_port,bound_account)
        if args.desktop:
            def control():
                for line in sys.stdin:
                    if line.strip()=='stop':break
                stopping.set()
                server.shutdown()
            threading.Thread(target=control,daemon=True).start()
        server.serve_forever()
    finally:
        server.server_close()
        if station:
            stopping.set()
            pool.shutdown(wait=True,cancel_futures=True)
        state_lease.close()

if __name__=='__main__':main()
