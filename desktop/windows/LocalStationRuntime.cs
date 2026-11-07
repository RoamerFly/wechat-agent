using System;
using System.Collections.Generic;
using System.Globalization;
using System.IO;
using System.Linq;
using System.Net.Http;
using System.Text;
using System.Threading;
using System.Threading.Tasks;

namespace WechatAgent.Desktop {
    // A local station owns only its own child processes.  A wx daemon is an
    // explicit owner opt-in: it is never discovered, adopted, or enrolled.
    internal sealed class LocalStationRuntime : IDisposable {
        readonly Deployment d;
        readonly HttpClient nativeApi = new HttpClient(new HttpClientHandler { UseProxy = false, AllowAutoRedirect = false }) { Timeout = TimeSpan.FromSeconds(3) };
        OwnedProcess server, native;
        ProcessJob job;
        FileStream nativeRunGate;
        string nativeServerId, nativeGeneration, nativeToken;
        // This marker is intentionally separate from the old-desktop cutover
        // evidence.  It proves only that this LocalStation launch owned and
        // cleanly stopped its own station/native pair.
        string nativeStopEpoch;
        bool nativeAdmitted, stationAdmitted;
        internal LocalStationRuntime(Deployment deployment) { d = deployment; }
        internal bool Running { get { return server != null && server.Alive; } }
        internal int PID { get { return server == null ? 0 : server.ID; } }
        internal int NativePID { get { return native == null ? 0 : native.ID; } }

        static bool Contains(string parent, string child) {
            parent = WindowsRuntime.ComparablePath(parent).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar;
            child = WindowsRuntime.ComparablePath(child).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar;
            return String.Equals(parent, child, StringComparison.OrdinalIgnoreCase) || child.StartsWith(parent, StringComparison.OrdinalIgnoreCase);
        }
        static void NoLinkedFile(string path) {
            if (File.Exists(path) && (File.GetAttributes(path) & FileAttributes.ReparsePoint) != 0) throw new InvalidOperationException("微信原生配置和令牌文件不能使用链接。");
        }
        void ValidateNativeLayout() {
            var nativeRoot = d.StationNativeRoot;
            var appRoot = Path.GetDirectoryName(nativeRoot);
            if (Contains(d.StationWorkspace, appRoot) || Contains(appRoot, d.StationWorkspace))
                throw new InvalidOperationException("本机成长站工作区不能与微信应用私有目录重叠。请在配置中设置独立 workspace_root。");
            // Old desktop native deployments have this exact root layout; a
            // station must never reinterpret it as its own private state.
            if (File.Exists(Path.Combine(d.Root, "api-token")) && Directory.Exists(Path.Combine(d.Root, "admin")))
                throw new InvalidOperationException("data_root 看起来是旧版微信原生数据目录，拒绝接管。请为本机成长站选择独立 data_root。");
            WindowsRuntime.NoLinks(nativeRoot);
            NoLinkedFile(Path.Combine(nativeRoot, "native-server.toml"));
            NoLinkedFile(Path.Combine(nativeRoot, "api-token"));
        }
        void AdmitNativeDirectories() {
            var nativeRoot = d.StationNativeRoot;
            WindowsRuntime.PrivateDirectory(Path.Combine(d.Root, "apps"));
            WindowsRuntime.PrivateDirectory(Path.Combine(d.Root, "apps", "wechat"));
            WindowsRuntime.PrivateDirectory(nativeRoot);
        }
        void RejectOfflineNativeRelease() {
            var marker = Path.Combine(WindowsRuntime.ComparablePath(d.Root), "station-native-release.json");
            try {
                var attributes = File.GetAttributes(marker);
                if ((attributes & FileAttributes.ReparsePoint) != 0)
                    throw new InvalidOperationException("本机成长站离线放行记录不能使用链接，拒绝启动微信原生服务。");
                throw new InvalidOperationException("此数据目录已记录离线放行意图，拒绝重新启动微信原生服务。请先由当前管理站核对并处理放行记录。");
            } catch (FileNotFoundException) {
                // No durable release intent: the lock holder may continue its
                // own start sequence.
            } catch (DirectoryNotFoundException error) {
                throw new InvalidOperationException("无法安全核对离线放行记录，拒绝启动微信原生服务。", error);
            } catch (UnauthorizedAccessException error) {
                throw new InvalidOperationException("无法安全读取离线放行记录，拒绝启动微信原生服务。", error);
            } catch (IOException error) {
                throw new InvalidOperationException("无法安全读取离线放行记录，拒绝启动微信原生服务。", error);
            }
        }
        void BeginNativeRun() {
            var path = Path.Combine(WindowsRuntime.ComparablePath(d.Root), "station-native-run.lock");
            NoLinkedFile(path);
            var gate = new FileStream(path, FileMode.OpenOrCreate, FileAccess.ReadWrite, FileShare.ReadWrite);
            try {
                try { gate.Lock(0, 1); }
                catch (IOException) { throw new InvalidOperationException("此本机成长站数据目录正在由另一个桌面实例使用（station-native-run.lock）。"); }
                // The Go station writes this durable intent before releasing
                // the old native claim.  Hold the same gate before checking so
                // a restarted desktop cannot erase the stop proof or create a
                // child in the release window.
                RejectOfflineNativeRelease();
                nativeRunGate = gate;
            } catch { gate.Dispose(); throw; }
        }
        void EndNativeRun() {
            var gate = nativeRunGate; nativeRunGate = null;
            if (gate == null) return;
            try { gate.Unlock(0, 1); }
            finally { gate.Dispose(); }
        }
        string NativeStopStatePath() { return Path.Combine(WindowsRuntime.ComparablePath(d.Root), "station-native-stop.json"); }
        void WriteNativeStopState(string state, string epoch, string serverId, string generation) {
            if (state != "starting" && state != "stopped") throw new ArgumentException("本机成长站停止状态无效。", "state");
            if (String.IsNullOrEmpty(epoch) || epoch.Length != 32 || !epoch.All(c => (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')))
                throw new ArgumentException("本机成长站停止标识无效。", "epoch");
            var dataRoot = WindowsRuntime.ComparablePath(d.Root);
            WindowsRuntime.NoLinks(dataRoot);
            var path = NativeStopStatePath();
            if (File.Exists(path) && (File.GetAttributes(path) & FileAttributes.ReparsePoint) != 0)
                throw new InvalidOperationException("本机成长站停止记录不能使用链接。");
            var temp = path + "." + Guid.NewGuid().ToString("N") + ".tmp";
            var text = Deployment.Json.Serialize(new {
                version = 1, state = state, epoch = epoch, data_root = dataRoot,
                native_server_id = serverId ?? "", native_ownership_generation = generation ?? "",
                at = DateTime.UtcNow.ToString("o", CultureInfo.InvariantCulture)
            });
            try {
                var bytes = new UTF8Encoding(false).GetBytes(text);
                using (var file = new FileStream(temp, FileMode.CreateNew, FileAccess.Write, FileShare.None, 4096, FileOptions.WriteThrough)) {
                    file.Write(bytes, 0, bytes.Length); file.Flush(true);
                }
                if (File.Exists(path)) File.Replace(temp, path, null); else File.Move(temp, path);
            } finally {
                if (File.Exists(temp)) File.Delete(temp);
            }
        }
        static void WriteNativeConfig(string nativeRoot) {
            var path = Path.Combine(nativeRoot, "native-server.toml");
            NoLinkedFile(path);
            if (!File.Exists(path)) using (var file = new FileStream(path, FileMode.CreateNew, FileAccess.Write, FileShare.Read)) {
                var bytes = new UTF8Encoding(false).GetBytes("schemaVersion = 1\n"); file.Write(bytes, 0, bytes.Length);
            }
            var text = File.ReadAllText(path, Encoding.UTF8);
            if (text.IndexOf("schemaVersion = 1", StringComparison.Ordinal) < 0)
                throw new InvalidOperationException("本机成长站微信配置版本不受支持；仅接受 schemaVersion = 1 的私有配置。");
        }
        internal void CheckAlive() {
            if (!Running) throw new InvalidOperationException("本机成长站提前退出，请查看 data/station/logs/station.log；没有改动微信或旧数据。");
            if (d.LocalStationNative && (native == null || !native.Alive)) throw new InvalidOperationException("本机成长站微信原生服务提前退出，请查看 data/station/logs/wechat-native.log；没有创建账号或修改旧数据。");
        }
        HttpRequestMessage NativeMessage(string path, object body) {
            var request = new HttpRequestMessage(body == null ? HttpMethod.Get : HttpMethod.Post, d.StationNativeUrl + path);
            if (String.IsNullOrEmpty(nativeToken)) {
                var token = Path.Combine(d.StationNativeRoot, "api-token"); NoLinkedFile(token);
                nativeToken = File.ReadAllText(token, Encoding.UTF8).Trim();
                if (nativeToken == "") throw new InvalidOperationException("微信原生服务令牌为空，拒绝连接。");
            }
            request.Headers.Authorization = new System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", nativeToken);
            if (body != null) request.Content = new StringContent(Deployment.Json.Serialize(body), Encoding.UTF8, "application/json");
            return request;
        }
        async Task<Dictionary<string, object>> NativeRequest(string path, object body) {
            using (var request = NativeMessage(path, body)) using (var response = await nativeApi.SendAsync(request)) {
                response.EnsureSuccessStatusCode();
                return Deployment.Json.Deserialize<Dictionary<string, object>>(await response.Content.ReadAsStringAsync());
            }
        }
        async Task WaitForNative(CancellationToken cancel) {
            var deadline = DateTime.UtcNow.AddSeconds(90);
            while (true) {
                cancel.ThrowIfCancellationRequested();
                if (native == null || !native.Alive) throw new InvalidOperationException("本机成长站微信原生服务启动失败，请查看 data/station/logs/wechat-native.log。没有创建或登录微信账号。");
                try {
                    var value = await NativeRequest("/v1/status", null);
                    if (Convert.ToInt32(value["pid"]) != native.ID || !String.Equals(WindowsRuntime.ComparablePath(Deployment.Text(value, "dataDir")), WindowsRuntime.ComparablePath(d.StationNativeRoot), StringComparison.OrdinalIgnoreCase))
                        throw new InvalidOperationException("微信原生服务实例身份或数据目录不匹配，拒绝接管。");
                    nativeServerId = Deployment.Text(value, "serverId"); nativeGeneration = Deployment.Text(value, "ownershipGeneration");
                    if (nativeServerId == "" || nativeGeneration == "") throw new InvalidOperationException("微信原生服务缺少归属信息，拒绝接管。");
                    nativeAdmitted = true;
                    return;
                } catch (HttpRequestException) { } catch (TaskCanceledException) { } catch (FileNotFoundException) { }
                // The owned daemon can still be writing its newly created token.
                catch (IOException error) {
                    if (error.HResult != unchecked((int)0x80070020) && error.HResult != unchecked((int)0x80070021)) throw;
                }
                if (DateTime.UtcNow > deadline) throw new InvalidOperationException("本机成长站微信原生服务未就绪，请检查 data/station/logs/wechat-native.log。");
                await Task.Delay(300, cancel);
            }
        }
        async Task<bool> StopNative(bool startupFailure) {
            var owned = native; native = null;
            if (owned == null) return false;
            Exception failure = null;
            var graceful = false;
            try {
                if (owned.Alive && nativeAdmitted && !String.IsNullOrEmpty(nativeServerId) && !String.IsNullOrEmpty(nativeGeneration)) {
                    await NativeRequest("/v1/daemon/shutdown", new { pid = owned.ID, serverId = nativeServerId, ownershipGeneration = nativeGeneration });
                    await owned.WaitStopped();
                    graceful = owned.ExitCode == 0;
                } else if (owned.Alive && startupFailure) {
                    owned.StopUnadmitted();
                    await owned.WaitStopped();
                }
                else if (owned.Alive) throw new InvalidOperationException("微信原生服务缺少启动时确认的归属信息，拒绝结束它。");
            } catch (Exception error) {
                failure = new InvalidOperationException("微信原生服务未能正常停止；仅此桌面实例拥有的进程将由进程隔离回收。请查看 data/station/logs/wechat-native.log。", error);
            } finally { owned.Dispose(); nativeAdmitted = false; nativeServerId = nativeGeneration = nativeToken = null; }
            if (failure != null) throw failure;
            return graceful;
        }
        internal async Task Start(CancellationToken cancel) {
            cancel.ThrowIfCancellationRequested();
            if (Running) { CheckAlive(); return; }
            if (server != null) { server.Dispose(); server = null; }
            if (job != null) { job.Dispose(); job = null; }
            if (!File.Exists(d.StationExe)) throw new InvalidOperationException("本机成长站程序不存在，请使用 LocalStation 预览包重新构建。");
            WindowsRuntime.NoLinks(d.StationExe); WindowsRuntime.NoLinks(d.Root);
            if (d.LocalStationNative) {
                if (!File.Exists(d.StationNativeExe)) throw new InvalidOperationException("本机成长站微信原生运行文件不存在，请使用包含 wx.exe 的 Windows 程序包。");
                WindowsRuntime.VerifyNativeBundle(d.StationNativeExe, true); ValidateNativeLayout();
            }
            // All potentially destructive validation has completed above.
            WindowsRuntime.PrivateDirectory(d.Root); Directory.CreateDirectory(Path.Combine(d.Root, "logs")); Directory.CreateDirectory(d.StationWorkspace); WindowsRuntime.NoLinks(d.StationWorkspace);
            WindowsRuntime.FreePort(d.Url);
            if (d.LocalStationNative) { AdmitNativeDirectories(); WriteNativeConfig(d.StationNativeRoot); WindowsRuntime.FreePort(d.StationNativeUrl); }
            job = new ProcessJob(); nativeServerId = nativeGeneration = nativeToken = null;
            nativeAdmitted = stationAdmitted = false;
            Exception startFailure = null;
            try {
                if (d.LocalStationNative) {
                    BeginNativeRun();
                    // A new launch invalidates any prior clean-stop evidence
                    // before either owned child is created.  Failed and forced
                    // starts deliberately leave this record at "starting".
                    nativeStopEpoch = Guid.NewGuid().ToString("N");
                    WriteNativeStopState("starting", nativeStopEpoch, "", "");
                    // Empty explicit environment: OwnedProcess clears WECHAT_/
                    // XDG_ values, and this child inherits no station secrets.
                    native = OwnedProcess.Start(d.StationNativeExe, new [] { "--json", "--server-config", Path.Combine(d.StationNativeRoot, "native-server.toml"), "--data-dir", d.StationNativeRoot, "--listen", d.StationNativeUrl.Substring("http://".Length), "serve" }, Path.GetDirectoryName(d.StationNativeExe), new Dictionary<string, string>(), Path.Combine(d.Root, "logs", "wechat-native.log"), job, true);
                    await WaitForNative(cancel);
                }
                var args = new List<string> { "--desktop", "--data", d.Root, "--workspace", d.StationWorkspace, "--listen", new Uri(d.Url).Authority };
                if (d.LocalStationNative) { args.Add("--wechat-native-url"); args.Add(d.StationNativeUrl); args.Add("--wechat-native-pid"); args.Add(native.ID.ToString()); }
                var programEnv = new Dictionary<string, string>();
                var media = Path.Combine(Path.GetDirectoryName(d.StationExe), "runtimes", "windows-media");
                var ffmpeg = Path.Combine(media, "ffmpeg.exe");
                var ffprobe = Path.Combine(media, "ffprobe.exe");
                if (File.Exists(ffmpeg) && File.Exists(ffprobe)
                    && ((File.GetAttributes(ffmpeg) | File.GetAttributes(ffprobe)) & FileAttributes.ReparsePoint) == 0) {
                    WindowsRuntime.NoLinks(media);
                    programEnv["PATH"] = media + Path.PathSeparator + Environment.GetEnvironmentVariable("PATH");
                }
                server = OwnedProcess.Start(d.StationExe, args.ToArray(), d.Workspace, programEnv, Path.Combine(d.Root, "logs", "station.log"), job);
                stationAdmitted = true;
            } catch (Exception error) {
                startFailure = error;
            }
            if (startFailure != null) {
                if (server != null) { server.Dispose(); server = null; }
                Exception cleanupFailure = null;
                try { await StopNative(true); } catch (Exception error) { cleanupFailure = error; }
                nativeStopEpoch = null;
                if (job != null) { job.Dispose(); job = null; }
                EndNativeRun();
                if (cleanupFailure != null) throw new InvalidOperationException("本机成长站启动失败，且已拥有的微信原生进程未能正常关闭。", new AggregateException(startFailure, cleanupFailure));
                throw startFailure;
            }
        }
        internal async Task Stop() {
            var ownedServer = server; server = null; Exception failure = null;
            var stopEpoch = nativeStopEpoch;
            var stoppedServerId = nativeServerId;
            var stoppedGeneration = nativeGeneration;
            var gracefulServer = false;
            var gracefulNative = false;
            try {
                // The station stops before its daemon, preventing new provider
                // work while the original authenticated daemon is shut down.
                if (ownedServer != null) {
                    var serverWasAlive = ownedServer.Alive;
                    if (serverWasAlive) await ownedServer.StopWeb(); else await ownedServer.WaitStopped();
                    if (ownedServer.ExitCode != 0) throw new InvalidOperationException("本机成长站以退出码 0x" + ownedServer.ExitCode.ToString("X8") + " 结束。");
                    gracefulServer = stationAdmitted && serverWasAlive;
                }
            } catch (Exception error) { failure = new InvalidOperationException("本机成长站未能正常完成关闭；微信原生服务仍将按本实例的归属信息停止。", error); }
            finally { if (ownedServer != null) ownedServer.Dispose(); }
            try { gracefulNative = await StopNative(false); } catch (Exception error) { if (failure == null) failure = error; }
            if (job != null) { job.Dispose(); job = null; }
            try {
                // Do not create evidence for a second stop, a partial launch,
                // a forced cleanup, or a failed/ambiguous native shutdown.
                if (failure == null && !String.IsNullOrEmpty(stopEpoch) && gracefulServer && gracefulNative)
                    WriteNativeStopState("stopped", stopEpoch, stoppedServerId, stoppedGeneration);
            } finally { nativeStopEpoch = null; stationAdmitted = false; EndNativeRun(); }
            if (failure != null) throw failure;
        }
        public void Dispose() { nativeApi.Dispose(); if (server != null) server.Dispose(); if (native != null) native.Dispose(); if (job != null) job.Dispose(); EndNativeRun(); }
    }
}
