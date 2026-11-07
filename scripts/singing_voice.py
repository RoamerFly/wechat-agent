"""Local singing timbre conversion. No Bailian calls or WeChat sends.

Run with the managed audio Python, a private JSON config and explicit paths.
Supported engines are pinned Seed-VC and an explicitly trained/pinned RVC voice.
Weights remain in the managed cache; inference never retrains a speaker model.
"""
from process_window import no_console_options
from singing_config import load_singing_config
import argparse
import gc
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time


def validate_singing_config(config):
    engine=config.get('engine','seed-vc')
    if engine=='rvc':
        for name in ('rvc_python','rvc_project','rvc_model','rvc_index'):
            if not isinstance(config.get(name),str) or not Path(config[name]).exists():raise ValueError('rvc_resource_missing')
        for name in ('rvc_model','rvc_index'):
            if hashlib.sha256(Path(config[name]).read_bytes()).hexdigest()!=config.get(name+'_sha256'):raise ValueError('rvc_resource_changed')
        rate,shift=config.get('index_rate',.35),config.get('semitones',0)
        if isinstance(rate,bool) or not isinstance(rate,(int,float)) or not 0<=rate<=1 or isinstance(shift,bool) or not isinstance(shift,int) or not -12<=shift<=12:raise ValueError('invalid_rvc_parameters')
        backing=config.get('accompaniment_semitones',0)
        protect=config.get('protect',.33)
        if isinstance(backing,bool) or not isinstance(backing,int) or not -12<=backing<=12 or (shift-backing)%12 or isinstance(protect,bool) or not isinstance(protect,(int,float)) or not 0<=protect<=.5:raise ValueError('incompatible_singing_key')
        if config.get('precision','auto') not in ('auto','float32'):raise ValueError('invalid_rvc_precision')
        if config.get('rvc_device','auto') not in ('auto','cpu','cuda:0'):raise ValueError('invalid_rvc_device')
    elif engine=='seed-vc':
        steps,cfg=config.get('diffusion_steps',30),config.get('inference_cfg_rate',.7)
        if isinstance(steps,bool) or not isinstance(steps,int) or not 10<=steps<=100 or isinstance(cfg,bool) or not isinstance(cfg,(int,float)) or not 0<=cfg<=1:raise ValueError('invalid_seed_parameters')
        reference=Path(config['reference']).resolve()
        if hashlib.sha256(reference.read_bytes()).hexdigest()!=config['reference_sha256']:raise ValueError('voice_reference_changed')
        revision=subprocess.check_output(**no_console_options(), args=['git','-C',str(Path(config['runtime'])/'seed-vc'),'rev-parse','HEAD'],text=True).strip()
        if revision!=config['source_revision']:raise ValueError('voice_runtime_revision_changed')
    else:raise ValueError('unknown_singing_voice_engine')
    return engine


def mix_converted(original, vocals, converted, accompaniment=None):
    import numpy as np
    if len(converted) < len(original) - 4410 or len(converted) > len(original) + 4410:
        raise ValueError('converted_duration_mismatch')
    converted = np.pad(converted[:len(original)], (0, max(0, len(original)-len(converted))))
    backing = original-vocals if accompaniment is None else np.asarray(accompaniment)
    if backing.shape != original.shape:raise ValueError('accompaniment_shape_mismatch')
    mixed = backing+converted[:, None]
    peak = float(np.abs(mixed).max())
    if not np.isfinite(mixed).all() or peak < 1e-6:
        raise ValueError('invalid_converted_audio')
    if peak > .98:
        mixed *= .98/peak
    return mixed


def transpose_accompaniment(backing, rate, semitones, work):
    """Shift musical key without changing tempo; never detune just the singer."""
    import soundfile as sf
    import numpy as np
    if semitones==0:return backing
    if isinstance(semitones,bool) or not isinstance(semitones,int) or not -12<=semitones<=12:raise ValueError('invalid_backing_shift')
    source,out=Path(work)/'backing-original.wav',Path(work)/'backing-transposed.wav'
    sf.write(source,backing,rate,subtype='FLOAT')
    ratio=2**(semitones/12)
    subprocess.run(['ffmpeg','-v','error','-nostdin','-y','-i',str(source),'-af','rubberband=pitch='+str(ratio)+':tempo=1','-ar',str(rate),'-c:a','pcm_f32le',str(out)],check=True,timeout=180,**no_console_options())
    shifted,actual_rate=sf.read(out,dtype='float32',always_2d=True)
    if actual_rate!=rate or shifted.shape[1]!=backing.shape[1] or abs(len(shifted)-len(backing))>rate//10 or not np.isfinite(shifted).all():raise ValueError('backing_conversion_invalid')
    return np.pad(shifted[:len(backing)],((0,max(0,len(backing)-len(shifted))),(0,0)))


def main():
    p = argparse.ArgumentParser()
    p.add_argument('--config', required=True)
    p.add_argument('--source', required=True)
    p.add_argument('--output', required=True)
    p.add_argument('--offline', action='store_true')
    args = p.parse_args()
    config = load_singing_config(args.config)
    started=time.monotonic();engine=validate_singing_config(config);timings={}
    home = Path(config['runtime']).resolve()
    repo = home/'seed-vc'
    source, output = Path(args.source).resolve(), Path(args.output).resolve()
    work = output.parent/(output.stem+'-voice-work')
    work.mkdir(parents=True, exist_ok=True)
    os.environ.update(HF_HOME=str(home/'hf-cache'), HF_HUB_CACHE=str(home/'hf-cache/hub'), TORCH_HOME=str(home/'torch-cache'), HF_HUB_OFFLINE='1')
    if engine=='seed-vc':sys.path[:0] = [str(home/'python-libs'), str(repo)]
    import numpy as np
    import soundfile as sf
    import torch
    if args.offline:
        def reject_download(*args, **kwargs):
            raise RuntimeError('rvc_offline_model_cache_missing')
        torch.hub.download_url_to_file=reject_download
    import torchaudio
    import huggingface_hub
    import huggingface_hub.constants
    # Decode with FFmpeg rather than Torchaudio's optional TorchCodec runtime.
    normalized = work/'original.wav'
    subprocess.run(['ffmpeg', '-v', 'error', '-nostdin', '-y', '-i', str(source), '-ac', '2', '-ar', '44100', str(normalized)], check=True, timeout=90,**no_console_options())
    original, rate = sf.read(normalized, dtype='float32', always_2d=True)
    if not 0 < len(original) <= rate*600:
        raise ValueError('song_duration_limit')
    waveform = torch.from_numpy(original.T.copy())
    timings['load_and_decode']=time.monotonic()-started;separation_started=time.monotonic()
    device = 'cuda' if torch.cuda.is_available() else 'cpu'
    print('Separating vocals locally', flush=True)
    bundle = torchaudio.pipelines.HDEMUCS_HIGH_MUSDB_PLUS
    model = bundle.get_model().to(device).eval()
    if bundle.sample_rate != rate:
        raise ValueError('separator_sample_rate_mismatch')
    # Short overlapping windows keep this usable on a 6 GB GPU.
    stems = torch.zeros((len(model.sources), 2, len(original)))
    weights = torch.zeros(len(original))
    chunk, overlap = rate*8, rate
    with torch.inference_mode():
        for start in range(0, len(original), chunk-overlap):
            end = min(start+chunk, len(original))
            block = waveform[:, start:end]
            reference_level = block.mean(0)
            std = reference_level.std().clamp_min(1e-5)
            mean = reference_level.mean()
            normalized_block=(block-mean)/std
            # Arbitrary final song lengths can violate HDemucs STFT stride
            # assumptions. Infer a full fixed window and retain only real audio.
            normalized_block=torch.nn.functional.pad(normalized_block,(0,chunk-normalized_block.shape[-1]))
            separated = model(normalized_block[None].to(device))[0,...,:end-start].cpu()*std
            # DC mean belongs to the mixture, not to every source.
            separated[model.sources.index('vocals')] += mean
            fade = torch.ones(end-start)
            if start: fade[:min(overlap,len(fade))] = torch.linspace(0,1,min(overlap,len(fade)))
            if end<len(original): fade[-overlap:] = torch.linspace(1,0,overlap)
            stems[:, :, start:end] += separated*fade
            weights[start:end] += fade
    vocals = (stems[model.sources.index('vocals')]/weights.clamp_min(1e-6)).T.numpy()
    sf.write(work/'vocals.wav', vocals.mean(1), rate, subtype='PCM_16')
    del model, waveform, stems, weights, separated
    gc.collect()
    if device == 'cuda': torch.cuda.empty_cache()
    timings['separate_vocals']=time.monotonic()-separation_started
    if engine=='rvc':
        conversion_started=time.monotonic();converted_path=work/'rvc-vocals.wav'
        subprocess.run([config['rvc_python'],str(Path(__file__).with_name('rvc_voice.py').resolve()),'--project',config['rvc_project'],'--model',config['rvc_model'],'--index',config['rvc_index'],'--source',str(work/'vocals.wav'),'--output',str(converted_path),'--index-rate',str(config.get('index_rate',.35)),'--semitones',str(config.get('semitones',0)),'--protect',str(config.get('protect',.33)),'--precision',config.get('precision','auto'),'--device',config.get('rvc_device','auto')],check=True,timeout=1800,**no_console_options())
        timings['rvc_load_and_convert']=time.monotonic()-conversion_started
        resampled=work/'rvc-vocals-44k.wav'
        subprocess.run(['ffmpeg','-v','error','-nostdin','-y','-i',str(converted_path),'-ac','1','-ar',str(rate),str(resampled)],check=True,timeout=90,**no_console_options())
        converted,converted_rate=sf.read(resampled,dtype='float32')
        if converted_rate!=rate or converted.ndim!=1:raise ValueError('converted_format_mismatch')
        backing=transpose_accompaniment(original-vocals,rate,config.get('accompaniment_semitones',0),work)
        mixed=mix_converted(original,vocals,converted,backing)
        sf.write(output,mixed,rate,subtype='PCM_16')
        timings['total']=time.monotonic()-started
        conversion_metadata=json.loads(converted_path.with_suffix('.rvc.json').read_text(encoding='utf-8'))
        output.with_suffix('.voice.json').write_text(json.dumps({'profile':config['profile'],'engine':'RVC','model_sha256':config['rvc_model_sha256'],'index_sha256':config['rvc_index_sha256'],'requested_device':config.get('rvc_device','auto'),'rvc_device':conversion_metadata['device'],'separation_device':device,'index_rate':config.get('index_rate',.35),'semitones':config.get('semitones',0),'accompaniment_semitones':config.get('accompaniment_semitones',0),'protect':config.get('protect',.33),'precision':conversion_metadata['precision'],'duration_seconds':len(mixed)/rate,'timings_seconds':timings,'bailian_calls':0},indent=2),encoding='utf-8')
        print('RVC singing conversion complete; seconds',round(timings['total'],2),'; no messages sent.',flush=True)
        return
    # Import after configuring all caches. Upstream's helper uses a relative
    # cache; replace only that resolver with our pinned offline cache lookup.
    os.chdir(repo)
    import inference
    os.environ['HF_HUB_CACHE']=str(home/'hf-cache/hub')
    def snapshot(repo_id):
        path=home/'hf-cache/hub'/('models--'+repo_id.replace('/','--'))/'snapshots'/config['model_revisions'][repo_id]
        if not path.is_dir():raise ValueError('pinned_model_missing')
        return str(path)
    # Upstream mutates HF_HUB_CACHE at import time. Explicit local snapshots
    # avoid that relative cache and prevent silent changes to a model revision.
    from modules.bigvgan import bigvgan
    from transformers import WhisperModel, AutoFeatureExtractor
    original_vocoder=bigvgan.BigVGAN.from_pretrained
    original_whisper=WhisperModel.from_pretrained
    original_features=AutoFeatureExtractor.from_pretrained
    bigvgan.BigVGAN.from_pretrained=staticmethod(lambda name,**kw:original_vocoder(snapshot('nvidia/bigvgan_v2_44khz_128band_512x'),**kw))
    WhisperModel.from_pretrained=staticmethod(lambda name,**kw:original_whisper(snapshot('openai/whisper-small'),**kw))
    AutoFeatureExtractor.from_pretrained=staticmethod(lambda name,**kw:original_features(snapshot('openai/whisper-small'),**kw))
    def strict_checkpoint(model,optimizer,path,**kwargs):
        params=torch.load(path,map_location='cpu',weights_only=True)['net']
        for key,module in model.items():
            incoming={k.removeprefix('module.'):v for k,v in params[key].items()}
            expected=module.state_dict()
            for buffer in ('estimator.input_pos','estimator.t_embedder.freqs'):
                if buffer in expected:incoming[buffer]=expected[buffer]
            # Extra legacy checkpoint tensors may be unused by this pinned
            # architecture; every active tensor must load with its exact shape.
            module.load_state_dict({k:v for k,v in incoming.items() if k in expected},strict=True)
        return model,optimizer,0,0
    inference.load_checkpoint=strict_checkpoint
    def pinned(repo_id, model_filename='pytorch_model.bin', config_filename=None):
        kwargs = dict(repo_id=repo_id, revision=config['model_revisions'][repo_id], cache_dir=str(home/'hf-cache/hub'), local_files_only=True)
        model_path = huggingface_hub.hf_hub_download(filename=model_filename, **kwargs)
        return (model_path, huggingface_hub.hf_hub_download(filename=config_filename, **kwargs)) if config_filename else model_path
    inference.load_custom_model_from_hf = pinned
    def save_audio(path, tensor, sample_rate, **kwargs):
        sf.write(path, tensor.detach().cpu().numpy().T, sample_rate, subtype='PCM_16')
    torchaudio.save = save_audio
    print('Converting singing voice locally with profile '+config['profile'], flush=True)
    # Never alter the source checkout or inherit arbitrary model/style options.
    os.chdir(repo)
    reference=Path(config['reference']).resolve();revision=config['source_revision']
    inference.main(argparse.Namespace(source=str(work/'vocals.wav'), target=str(reference), output=str(work), diffusion_steps=config.get('diffusion_steps',30), length_adjust=1.0, inference_cfg_rate=config.get('inference_cfg_rate',.7), f0_condition=True, auto_f0_adjust=False, semi_tone_shift=0, checkpoint=None, config=None, fp16=device=='cuda'))
    converted_paths = list(work.glob('vc_*.wav'))
    if len(converted_paths)!=1: raise ValueError('converted_output_ambiguous')
    converted, converted_rate = sf.read(converted_paths[0], dtype='float32')
    if converted_rate!=rate or converted.ndim!=1: raise ValueError('converted_format_mismatch')
    mixed = mix_converted(original, vocals, converted)
    sf.write(output, mixed, rate, subtype='PCM_16')
    output.with_suffix('.voice.json').write_text(json.dumps({'profile':config['profile'],'reference_sha256':config['reference_sha256'],'engine':'Seed-VC','source_revision':revision,'diffusion_steps':config.get('diffusion_steps',30),'inference_cfg_rate':config.get('inference_cfg_rate',.7),'duration_seconds':len(mixed)/rate,'bailian_calls':0}, indent=2), encoding='utf-8')
    print('Singing conversion complete; no messages sent.', flush=True)


if __name__ == '__main__':
    main()
