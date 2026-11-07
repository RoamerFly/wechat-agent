using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.Globalization;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Net.NetworkInformation;
using System.Runtime.InteropServices;
using System.Security.AccessControl;
using System.Security.Cryptography;
using System.Security.Principal;
using System.Text;
using System.Threading;
using System.Threading.Tasks;
using Microsoft.Win32.SafeHandles;

namespace WechatAgent.Desktop {
    // The desktop owns foreground processes, rather than nesting a detached
    // daemon inside redirected pipes. A Job Object also owns their descendants.
    internal sealed class WindowsRuntime : IDisposable {
        readonly Deployment d;
        readonly ICommandRunner commands;
        readonly HttpClient api = new HttpClient(new HttpClientHandler { UseProxy = false, AllowAutoRedirect = false }) { Timeout = TimeSpan.FromSeconds(3) };
        OwnedProcess native, web;
        ProcessJob job;
        FileStream cutoverGate;
        string serverId, generation, cutoverEpoch;
        internal WindowsRuntime(Deployment config, ICommandRunner runner) { d = config; commands = runner; }
        internal bool WebRunning { get { return web != null && web.Alive; } }
        internal bool AnyRunning { get { return WebRunning || native != null && native.Alive; } }
        internal static string ComparablePath(string value) {
            if (value.StartsWith(@"\\?\UNC\", StringComparison.OrdinalIgnoreCase)) value = @"\\" + value.Substring(8);
            else if (value.StartsWith(@"\\?\", StringComparison.Ordinal)) value = value.Substring(4);
            return Path.GetFullPath(value).TrimEnd(Path.DirectorySeparatorChar);
        }
        internal static string LegacyCutoverStatePath(string root) {
            return Path.Combine(ComparablePath(root), "legacy-cutover-state.json");
        }
        internal static FileStream BeginLegacyCutoverRun(string root, string accountId, string epoch) {
            root = ComparablePath(root);
            NoLinks(root);
            var lockPath = Path.Combine(root, "legacy-cutover.lock");
            if (File.Exists(lockPath) && (File.GetAttributes(lockPath) & FileAttributes.ReparsePoint) != 0)
                throw new InvalidOperationException("旧桌面交接锁不能使用链接。");
            var gate = new FileStream(lockPath, FileMode.OpenOrCreate, FileAccess.ReadWrite, FileShare.ReadWrite);
            try {
                try { gate.Lock(0, 1); }
                catch (IOException) { throw new InvalidOperationException("此微信数据目录仍由旧桌面占用，或正在核对交接。"); }
                try {
                    File.GetAttributes(Path.Combine(root, "legacy-cutover-claim.json"));
                    throw new InvalidOperationException("这个微信数据目录已经交接给成长站。请先按回退流程停机并核对投递记录。");
                } catch (FileNotFoundException) { }
                WriteLegacyCutoverState(root, accountId, "starting", epoch, "");
                return gate;
            } catch { gate.Dispose(); throw; }
        }
        // The marker belongs to the desktop that controlled the legacy pair
        // or held the same native ownership lease without starting services.
        internal static void WriteLegacyCutoverState(string root, string accountId, string state, string epoch, string nativeServerId, string mode = null) {
            if (state != "starting" && state != "stopped") throw new ArgumentException("切换状态无效。", "state");
            if (mode != null && (mode != "offline_hold" || state != "stopped")) throw new ArgumentException("切换模式无效。", "mode");
            if (String.IsNullOrEmpty(accountId) || String.IsNullOrEmpty(epoch) || epoch.Length != 32 || !epoch.All(c => (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')))
                throw new ArgumentException("切换标识无效。", "epoch");
            var dataRoot = ComparablePath(root);
            NoLinks(dataRoot);
            var path = LegacyCutoverStatePath(dataRoot);
            if (File.Exists(path) && (File.GetAttributes(path) & FileAttributes.ReparsePoint) != 0)
                throw new InvalidOperationException("旧桌面切换记录不能使用链接。");
            var temp = path + "." + Guid.NewGuid().ToString("N") + ".tmp";
            var record = new Dictionary<string, object> {
                { "version", 2 }, { "state", state }, { "epoch", epoch }, { "account_id", accountId },
                { "data_root", dataRoot }, { "native_server_id", nativeServerId ?? "" },
                { "at", DateTime.UtcNow.ToString("o", CultureInfo.InvariantCulture) }
            };
            if (mode != null) record.Add("mode", mode);
            var text = Deployment.Json.Serialize(record);
            try {
                var bytes = new UTF8Encoding(false).GetBytes(text);
                using (var file = new FileStream(temp, FileMode.CreateNew, FileAccess.Write, FileShare.None, 4096, FileOptions.WriteThrough)) {
                    file.Write(bytes, 0, bytes.Length); file.Flush(true);
                }
                // Move and Replace are same-volume atomic operations.  A failed
                // replacement is safer than accepting an ambiguous stop proof.
                if (File.Exists(path)) File.Replace(temp, path, null); else File.Move(temp, path);
            } finally {
                if (File.Exists(temp)) File.Delete(temp);
            }
        }
        internal static async Task PrepareOfflineCutover(Deployment d, CancellationToken cancel) {
            if (!d.Windows) throw new InvalidOperationException("离线交接只适用于旧 Windows 原生微信部署。");
            cancel.ThrowIfCancellationRequested();
            var root = ComparablePath(d.Root);
            var drive = Path.GetPathRoot(root);
            if (drive == null || drive.Length != 3 || new DriveInfo(drive).DriveType != DriveType.Fixed || !Directory.Exists(root))
                throw new InvalidOperationException("离线交接需要已存在的本机固定卷数据目录。");
            NoLinks(root);
            var user = WindowsIdentity.GetCurrent().User;
            if (!user.Equals(Directory.GetAccessControl(root, AccessControlSections.Owner).GetOwner(typeof(SecurityIdentifier))))
                throw new InvalidOperationException("旧微信数据目录不属于当前用户。");
            foreach (var name in new[] { "owner.lock", "registry.sqlite" }) {
                var path = Path.Combine(root, name);
                if (!File.Exists(path) || (File.GetAttributes(path) & (FileAttributes.Directory | FileAttributes.Device | FileAttributes.ReparsePoint)) != 0)
                    throw new InvalidOperationException("离线交接需要已有且非链接的原生锁和注册库。");
            }
            var nativeExe = OfflineNativeExecutable(d.NativeExe);
            EnsureLegacyPortsIdle(d);
            var epoch = Guid.NewGuid().ToString("N");
            using (BeginLegacyCutoverRun(root, d.Account, epoch)) {
                var info = HiddenRunner.Info(nativeExe, new[] { "--data-dir", root, "--account", d.Account, "cutover-hold" });
                info.WorkingDirectory = Path.GetDirectoryName(nativeExe);
                OwnedProcess.ClearInheritedEnvironment(info, true);
                using (var child = new Process { StartInfo = info })
                using (var job = new ProcessJob()) {
                    cancel.ThrowIfCancellationRequested();
                    child.Start();
                    try { job.Assign(child); }
                    catch { if (!child.HasExited) child.Kill(); throw; }
                    var stderr = child.StandardError.BaseStream.CopyToAsync(Stream.Null);
                    var server = await ReadOfflineNativeReady(child, cancel).ConfigureAwait(false);
                    cancel.ThrowIfCancellationRequested();
                    if (child.HasExited) throw new InvalidOperationException("原生离线占用已提前退出。");
                    child.StandardInput.WriteLine("stop");
                    child.StandardInput.Flush();
                    child.StandardInput.Close();
                    var deadline = Task.Delay(TimeSpan.FromSeconds(30), cancel);
                    const string stopTimeout = "原生离线占用未按时结束，未签发停机凭证。";
                    var outputEnd = child.StandardOutput.ReadAsync(new char[1], 0, 1);
                    await WaitOfflinePipe(outputEnd, deadline, cancel, stopTimeout).ConfigureAwait(false);
                    if (await outputEnd.ConfigureAwait(false) != 0) throw new InvalidOperationException("原生离线占用返回了多余的状态输出。");
                    while (!child.HasExited) {
                        await WaitOfflinePipe(Task.Delay(50, cancel), deadline, cancel, stopTimeout).ConfigureAwait(false);
                    }
                    await WaitOfflinePipe(stderr, deadline, cancel, stopTimeout).ConfigureAwait(false);
                    if (child.ExitCode != 0) throw new InvalidOperationException("原生离线占用未正常结束，未签发停机凭证。");
                    cancel.ThrowIfCancellationRequested();
                    EnsureLegacyPortsIdle(d);
                    WriteLegacyCutoverState(root, d.Account, "stopped", epoch, server, "offline_hold");
                }
            }
        }
        static string OfflineNativeExecutable(string legacyExe) {
            var dir = Path.GetDirectoryName(legacyExe);
            NoLinks(dir);
            var launcherHash = NativeFileHash(typeof(WindowsRuntime).Assembly.Location).ToLowerInvariant();
            var pairName = "cutover-native-" + launcherHash;
            // Enumerate rather than using Directory.Exists, which reports false
            // for files and inaccessible or broken directory links as well.
            var matches = Directory.EnumerateFileSystemEntries(dir, pairName)
                .Where(path => String.Equals(Path.GetFileName(path), pairName, StringComparison.OrdinalIgnoreCase)).ToArray();
            if (matches.Length == 0) {
                VerifyNativeBundle(legacyExe);
                return legacyExe;
            }
            if (matches.Length != 1 ||
                (File.GetAttributes(matches[0]) & (FileAttributes.Directory | FileAttributes.Device | FileAttributes.ReparsePoint)) != FileAttributes.Directory)
                throw new InvalidOperationException("离线交接配套运行包路径无效或包含链接。");
            var pairedExe = Path.Combine(matches[0], "wx.exe");
            VerifyNativeBundle(pairedExe, cutoverLauncherHash: launcherHash);
            return pairedExe;
        }
        static void EnsureLegacyPortsIdle(Deployment d) {
            var adminPort = new Uri(d.Url).Port;
            var nativePort = new Uri(d.NativeUrl).Port;
            if (IPGlobalProperties.GetIPGlobalProperties().GetActiveTcpListeners().Any(x => x.Port == adminPort || x.Port == nativePort))
                throw new InvalidOperationException("旧部署的管理或原生端口仍有监听。请先正常退出旧桌面。");
        }
        static async Task WaitOfflinePipe(Task operation, Task deadline, CancellationToken cancel, string timeout) {
            if (await Task.WhenAny(operation, deadline).ConfigureAwait(false) != operation) {
                cancel.ThrowIfCancellationRequested();
                throw new InvalidOperationException(timeout);
            }
            await operation.ConfigureAwait(false);
        }
        static async Task<string> ReadOfflineNativeReady(Process child, CancellationToken cancel) {
            var deadline = Task.Delay(TimeSpan.FromSeconds(30), cancel);
            var line = new StringBuilder();
            var buffer = new char[1];
            while (line.Length < 4096) {
                var read = child.StandardOutput.ReadAsync(buffer, 0, buffer.Length);
                await WaitOfflinePipe(read, deadline, cancel, "原生离线占用未按时就绪。请检查原生运行包、数据锁和数据库状态。").ConfigureAwait(false);
                if (await read.ConfigureAwait(false) == 0)
                    throw new InvalidOperationException("原生离线占用未就绪。请检查原生运行包、数据锁和数据库状态。");
                if (buffer[0] == '\n') return ValidateOfflineNativeReady(line.ToString(), child.Id);
                line.Append(buffer[0]);
            }
            throw new InvalidOperationException("原生离线占用的状态输出超长。");
        }
        static string ValidateOfflineNativeReady(string line, int pid) {
            Dictionary<string, object> status;
            try { status = Deployment.Json.Deserialize<Dictionary<string, object>>(line); }
            catch (ArgumentException) { throw new InvalidOperationException("原生离线占用返回了无效的状态输出。"); }
            catch (InvalidOperationException) { throw new InvalidOperationException("原生离线占用返回了无效的状态输出。"); }
            object state, mode, dataset;
            Guid nativeId;
            if (status == null || !status.TryGetValue("state", out state) || (state as string) != "ready" ||
                !status.TryGetValue("mode", out mode) || (mode as string) != "cutover_hold" ||
                !status.TryGetValue("serverId", out dataset) || !Guid.TryParseExact(dataset as string, "D", out nativeId) || nativeId == Guid.Empty ||
                OfflineCount(status, "pid") != pid || OfflineCount(status, "accountCount") < 1)
                throw new InvalidOperationException("原生离线占用的进程、模式或数据集身份无法核验。");
            var counts = new[] { "pendingText", "pendingMedia", "uncertainText", "uncertainMedia" }.Select(key => OfflineCount(status, key)).ToArray();
            if (counts.Any(value => value != 0))
                throw new InvalidOperationException("原生账本仍有排队或结果未定的投递。请先按原任务核对，未签发停机凭证。");
            return (string)dataset;
        }
        static long OfflineCount(Dictionary<string, object> status, string key) {
            object value;
            if (status.TryGetValue(key, out value) && (value is int || value is long)) {
                var count = Convert.ToInt64(value, CultureInfo.InvariantCulture);
                if (count >= 0) return count;
            }
            throw new InvalidOperationException("原生离线占用的状态缺少有效的整数计数。");
        }
        internal void CheckAlive() {
            if (native == null || !native.Alive || web == null || !web.Alive) throw new InvalidOperationException("Windows 服务提前退出，请查看 data/logs；没有重放发送或清空数据。");
        }
        internal static void NoLinks(string path) {
            for (var item = new DirectoryInfo(Path.GetFullPath(path)); item != null; item = item.Parent)
                if (item.Exists && (item.Attributes & FileAttributes.ReparsePoint) != 0) throw new InvalidOperationException("数据和程序路径不能穿过链接或重解析点。");
        }
        internal static void PrivateDirectory(string path) {
            NoLinks(path);
            var user = WindowsIdentity.GetCurrent().User;
            // Elevated Windows tokens may default new ownership to Administrators.
            // Explicit ownership must agree with the native backend's user SID.
            var exists = Directory.Exists(path);
            if (exists && !user.Equals(Directory.GetAccessControl(path, AccessControlSections.Owner).GetOwner(typeof(SecurityIdentifier))))
                throw new InvalidOperationException("数据目录不属于当前用户，拒绝接管或自动更改所有者。");
            var acl = new DirectorySecurity();
            if (!exists) acl.SetOwner(user);
            acl.SetAccessRuleProtection(true, false);
            var inherit = InheritanceFlags.ContainerInherit | InheritanceFlags.ObjectInherit;
            acl.AddAccessRule(new FileSystemAccessRule(user, FileSystemRights.FullControl, inherit, PropagationFlags.None, AccessControlType.Allow));
            acl.AddAccessRule(new FileSystemAccessRule(new SecurityIdentifier(WellKnownSidType.LocalSystemSid, null), FileSystemRights.FullControl, inherit, PropagationFlags.None, AccessControlType.Allow));
            if (!exists) Directory.CreateDirectory(path, acl); else Directory.SetAccessControl(path, acl);
        }
        static string NativeFileHash(string path) {
            using (var stream = File.OpenRead(path)) using (var sha = SHA256.Create())
                return BitConverter.ToString(sha.ComputeHash(stream)).Replace("-", "");
        }
        static readonly HashSet<string> CutoverRuntimeFiles = new HashSet<string>(StringComparer.OrdinalIgnoreCase) {
            "wx.exe", "concrt140.dll", "msvcp140.dll", "msvcp140_1.dll", "msvcp140_2.dll", "msvcp140_atomic_wait.dll",
            "msvcp140_codecvt_ids.dll", "vccorlib140.dll", "vcruntime140.dll", "vcruntime140_1.dll", "vcruntime140_threads.dll"
        };
        internal static void VerifyNativeBundle(string nativeExe, bool stationRuntime = false, string cutoverLauncherHash = null) {
            bool cutoverRuntime = cutoverLauncherHash != null;
            bool strictInventory = stationRuntime || cutoverRuntime;
            var dir = Path.GetDirectoryName(nativeExe); NoLinks(dir);
            var manifestPath = Path.Combine(dir, "windows-native-manifest.json");
            if ((File.GetAttributes(manifestPath) & (FileAttributes.ReparsePoint | FileAttributes.Device | FileAttributes.Directory)) != 0)
                throw new InvalidOperationException("运行清单必须为非链接的普通文件。");
            var manifest = Deployment.Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(manifestPath, Encoding.UTF8));
            if (manifest == null) throw new InvalidOperationException("Windows 原生运行清单无效。");
            if ((File.GetAttributes(nativeExe) & (FileAttributes.ReparsePoint | FileAttributes.Device | FileAttributes.Directory)) != 0)
                throw new InvalidOperationException("原生运行文件必须为非链接的普通文件。");
            // The station package deliberately omits wechat-admin.exe.  Its
            // manifest still enumerates every bundled native dependency, so extra
            // files cannot silently enter the verified runtime directory.
            object listed; var hasFiles = manifest.TryGetValue("files", out listed) && listed is Dictionary<string, object>;
            if (stationRuntime && (!hasFiles || !manifest.ContainsKey("local_station_runtime") || !Convert.ToBoolean(manifest["local_station_runtime"]))) throw new InvalidOperationException("本机成长站微信运行包清单不完整或来源不匹配。");
            if (cutoverRuntime) {
                object version, platform, mode, launcher, nativeHash;
                if (!hasFiles || !manifest.TryGetValue("schema_version", out version) || !Object.Equals(version, 1) ||
                    !manifest.TryGetValue("platform", out platform) || (platform as string) != "windows-x64" ||
                    !manifest.TryGetValue("cutover_runtime", out mode) || !Object.Equals(mode, true) ||
                    !manifest.TryGetValue("launcher_sha256", out launcher) || !String.Equals(launcher as string, cutoverLauncherHash, StringComparison.OrdinalIgnoreCase) ||
                    !manifest.TryGetValue("native_sha256", out nativeHash) || !(nativeHash is string))
                    throw new InvalidOperationException("离线交接配套运行包清单不完整或与当前启动器不匹配。");
            }
            if (!String.Equals(NativeFileHash(nativeExe), Deployment.Text(manifest, "native_sha256"), StringComparison.OrdinalIgnoreCase))
                throw new InvalidOperationException("Windows 运行文件校验失败，请重新构建完整程序包；用户数据已保留。");
            if (hasFiles) {
                var files = (Dictionary<string, object>)listed;
                if (files.Count == 0) throw new InvalidOperationException("Windows 原生运行文件清单为空。");
                var remaining = cutoverRuntime ? new HashSet<string>(files.Keys, StringComparer.OrdinalIgnoreCase) : null;
                if (remaining != null && remaining.Count != files.Count)
                    throw new InvalidOperationException("离线交接配套运行包登记了重复文件。");
                var nativeListed = false;
                foreach (var entry in files) {
                    if (stationRuntime && (entry.Key != Path.GetFileName(entry.Key) || entry.Key.IndexOf(':') >= 0)) throw new InvalidOperationException("本机成长站微信运行包只能登记同级文件。");
                    if (cutoverRuntime && !CutoverRuntimeFiles.Contains(entry.Key))
                        throw new InvalidOperationException("离线交接配套运行包只能登记 wx.exe 和允许的 Windows 运行库。");
                    var target = Path.GetFullPath(Path.Combine(dir, entry.Key));
                    if (!target.StartsWith(Path.GetFullPath(dir).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase)) throw new InvalidOperationException("Windows 原生运行文件清单包含越界路径。");
                    NoLinks(target);
                    if (strictInventory && (File.GetAttributes(target) & (FileAttributes.Directory | FileAttributes.Device | FileAttributes.ReparsePoint)) != 0)
                        throw new InvalidOperationException("Windows 原生运行包不能包含链接或目录。");
                    if (!String.Equals(NativeFileHash(target), cutoverRuntime ? entry.Value as string : Convert.ToString(entry.Value), StringComparison.OrdinalIgnoreCase))
                        throw new InvalidOperationException("Windows 原生运行文件校验失败，请重新构建完整程序包；用户数据已保留。");
                    if (String.Equals(target, Path.GetFullPath(nativeExe), StringComparison.OrdinalIgnoreCase)) nativeListed = true;
                }
                if (strictInventory && !nativeListed) throw new InvalidOperationException("Windows 原生运行包未登记 wx.exe。");
                if (cutoverRuntime) {
                    remaining.Add("windows-native-manifest.json");
                    foreach (var path in Directory.EnumerateFileSystemEntries(dir)) {
                        if (!remaining.Remove(Path.GetFileName(path)))
                            throw new InvalidOperationException("离线交接配套运行包包含未登记或重复的文件或目录。");
                    }
                    if (remaining.Count != 0) throw new InvalidOperationException("离线交接配套运行包缺少已登记文件。");
                }
                if (stationRuntime) foreach (var path in Directory.GetFileSystemEntries(dir)) {
                    var name = Path.GetFileName(path);
                    if (name != "windows-native-manifest.json" && !files.ContainsKey(name)) throw new InvalidOperationException("本机成长站微信运行包包含未登记的文件或目录。");
                }
            }
        }
        internal static void VerifyBundle(Deployment config) {
            var dir = Path.GetDirectoryName(config.NativeExe); NoLinks(config.Root);
            VerifyNativeBundle(config.NativeExe);
            var manifest = Deployment.Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(Path.Combine(dir, "windows-native-manifest.json"), Encoding.UTF8));
            var admin = config.AdminExe;
            if ((File.GetAttributes(admin) & FileAttributes.ReparsePoint) != 0) throw new InvalidOperationException("运行文件不能使用链接。");
            using (var stream = File.OpenRead(admin)) using (var sha = SHA256.Create())
                if (!String.Equals(BitConverter.ToString(sha.ComputeHash(stream)).Replace("-", ""), Deployment.Text(manifest, "admin_sha256"), StringComparison.OrdinalIgnoreCase)) throw new InvalidOperationException("Windows 运行文件校验失败，请重新构建完整程序包；用户数据已保留。");
            var resources = Path.GetFullPath(Path.Combine(dir, ".."));
            foreach (var name in new [] { "windows-python-cli", "windows-media", "windows-music" }) {
                var folder = Path.Combine(resources, name);
                if (!Directory.Exists(folder)) continue; // earlier native-only packages remain usable
                VerifyResourceDirectory(folder);
            }
        }
        internal static void VerifyResourceDirectory(string folder) {
            NoLinks(folder);
            var record = Deployment.Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(Path.Combine(folder, "runtime-manifest.json"), Encoding.UTF8));
            var files = record["files"] as Dictionary<string, object>;
            if (files == null || files.Count == 0) throw new InvalidOperationException("托管运行环境清单为空。");
            foreach (var entry in files) {
                var target = Path.GetFullPath(Path.Combine(folder, entry.Key));
                if (!target.StartsWith(Path.GetFullPath(folder).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase)) throw new InvalidOperationException("托管运行环境清单包含越界路径。");
                NoLinks(target);
                using (var stream = File.OpenRead(target)) using (var sha = SHA256.Create())
                    if (!String.Equals(BitConverter.ToString(sha.ComputeHash(stream)).Replace("-", ""), Convert.ToString(entry.Value), StringComparison.OrdinalIgnoreCase)) throw new InvalidOperationException("项目内运行环境校验失败；请修复该环境，用户资料已保留。");
            }
        }
        internal static void FreePort(string url) {
            var uri = new Uri(url); var listener = new System.Net.Sockets.TcpListener(IPAddress.Loopback, uri.Port);
            try { listener.Server.ExclusiveAddressUse = true; listener.Start(); }
            catch (System.Net.Sockets.SocketException) { throw new InvalidOperationException("本机端口 " + uri.Port + " 已占用；拒绝接管或结束其他服务。"); }
            finally { listener.Stop(); }
        }
        string[] NativeArgs(params string[] tail) {
            return new [] { "--json", "--server-config", Path.Combine(d.Root, "native-server.toml"), "--data-dir", d.Root, "--listen", new Uri(d.NativeUrl).Authority }.Concat(tail).ToArray();
        }
        async Task FinishRemovedAccounts(CancellationToken cancel) {
            if (!File.Exists(Path.Combine(d.Root, "admin", "accounts.json"))) return;
            var result = await commands.Run(d.AdminExe, new [] { "desktop-clean-accounts", d.Root, d.Account }, cancel);
            if (result.Code != 0) throw new InvalidOperationException("未能完成已删除账号的本机数据清理，请检查数据目录是否被占用后重试。");
        }
        internal async Task Start(CancellationToken cancel) {
            VerifyBundle(d);
            if (AnyRunning) { CheckAlive(); return; }
            // A crashed process can still own its log/pipe handles. Release
            // the stopped instance before retrying, without adopting any PID.
            if (native != null || web != null || cutoverGate != null) await Stop();
            FreePort(d.Url); FreePort(d.NativeUrl);
            PrivateDirectory(d.Root);
            // Claim and starting-marker publication share an OS lock with the
            // new station. No legacy runtime work may precede this check.
            cutoverEpoch = Guid.NewGuid().ToString("N");
            cutoverGate = BeginLegacyCutoverRun(d.Root, d.Account, cutoverEpoch);
            PrivateDirectory(Path.Combine(d.Workspace, "secrets"));
            Directory.CreateDirectory(Path.Combine(d.Root, "logs"));
            // Explicit config avoids inheriting another installed wx instance's
            // defaults. Existing config is preserved and only read by wx.
            var serverConfig = Path.Combine(d.Root, "native-server.toml");
            if (!File.Exists(serverConfig)) using (var f = new FileStream(serverConfig, FileMode.CreateNew)) {
                var bytes = Encoding.UTF8.GetBytes("schemaVersion = 1\n"); f.Write(bytes, 0, bytes.Length);
            }
            var adminDir = Path.Combine(d.Root, "admin"); Directory.CreateDirectory(adminDir);
            if (!File.Exists(Path.Combine(adminDir, "admin-token")) && !File.Exists(Path.Combine(adminDir, "admin-password"))) {
                var bytes = new byte[32]; using (var rng = RandomNumberGenerator.Create()) rng.GetBytes(bytes);
                using (var f = new FileStream(Path.Combine(adminDir, "admin-token"), FileMode.CreateNew)) {
                    var text = Encoding.UTF8.GetBytes(Convert.ToBase64String(bytes)); f.Write(text, 0, text.Length);
                }
            }
            job = new ProcessJob();
            serverId = generation = null;
            var resources = Path.GetFullPath(Path.Combine(Path.GetDirectoryName(d.NativeExe), ".."));
            var media = Path.Combine(resources, "windows-media");
            var programEnv = new Dictionary<string, string>();
            if (File.Exists(Path.Combine(media, "ffmpeg.exe")) && File.Exists(Path.Combine(media, "ffprobe.exe")))
                programEnv["PATH"] = media + Path.PathSeparator + Environment.GetEnvironmentVariable("PATH");
            await FinishRemovedAccounts(cancel);
            native = OwnedProcess.Start(d.NativeExe, NativeArgs("serve"), d.Workspace, programEnv, Path.Combine(d.Root, "logs", "native.log"), job);
            var deadline = DateTime.UtcNow.AddSeconds(90);
            while (true) {
                cancel.ThrowIfCancellationRequested();
                if (!native.Alive) throw new InvalidOperationException("Windows 微信后台启动失败（退出码 0x" + native.ExitCode.ToString("X8") + "），请查看 data/logs/native.log。若为 0xC0000135，请检查 Microsoft Visual C++ x64 运行库。");
                try {
                    var value = await NativeRequest("/v1/status", null);
                    if (Convert.ToInt32(value["pid"]) != native.ID || !String.Equals(ComparablePath(Deployment.Text(value, "dataDir")), ComparablePath(d.Root), StringComparison.OrdinalIgnoreCase)) throw new InvalidOperationException("微信后台实例身份不匹配，拒绝接管。");
                    serverId = Deployment.Text(value, "serverId"); generation = Deployment.Text(value, "ownershipGeneration");
                    if (serverId == "" || generation == "") throw new InvalidOperationException("微信后台缺少实例归属信息，拒绝接管。");
                    break;
                } catch (HttpRequestException) { } catch (TaskCanceledException) { } catch (FileNotFoundException) { }
                if (DateTime.UtcNow > deadline) throw new InvalidOperationException("Windows 微信后台未就绪，请检查日志。");
                await Task.Delay(300, cancel);
            }
            using (var request = NativeMessage("/v1/accounts/" + Uri.EscapeDataString(d.Account), null)) using (var response = await api.SendAsync(request)) {
                if (response.StatusCode == HttpStatusCode.NotFound) await NativeRequest("/v1/accounts", new { id = d.Account, alias = d.Account });
                else if (!response.IsSuccessStatusCode) throw new InvalidOperationException("微信账号状态无法核验，没有自动重新创建。");
            }
            var env = new Dictionary<string, string> {
                { "WECHAT_ADMIN_DATA_DIR", adminDir }, { "WECHAT_BOT_STATE_DIR", Path.Combine(d.Root, "bot-state") },
                { "WECHAT_ADMIN_LISTEN", new Uri(d.Url).Authority }, { "WECHAT_ADMIN_ORIGIN", d.Url },
                { "WECHAT_NATIVE_URL", d.NativeUrl }, { "WECHAT_NATIVE_TOKEN_FILE", Path.Combine(d.Root, "api-token") },
                { "WECHAT_NATIVE_ACCOUNT", d.Account }, { "WECHAT_WORKSPACE_ROOT", Path.Combine(d.Root, "managed-workspaces") }
            };
            foreach (var pair in programEnv) env[pair.Key] = pair.Value;
            var pythonw = Path.Combine(resources, "windows-python-cli", "pythonw.exe");
            var helper = Path.Combine(resources, "windows-agent-host", "windows_agent_host.pyw");
            if (File.Exists(pythonw) && File.Exists(helper)) env["WECHAT_WINDOWS_BROKER_CONFIG"] = Deployment.Json.Serialize(new {
                pythonw = pythonw, helper = helper, spool = Path.Combine(adminDir, "windows-broker-spool")
            });
            var pluginHelper = Path.Combine(resources, "windows-agent-host", "windows_plugin_host.pyw");
            var documentRuntime = Path.Combine(resources, "document-workshop");
            var deviceToolsRoots = new HashSet<string>(StringComparer.OrdinalIgnoreCase);
            foreach (var directory in new [] {
                Path.Combine(d.Workspace, "runtimes", "android-platform-tools"),
                Path.Combine(resources, "android-platform-tools")
            }) if (Directory.Exists(directory)) deviceToolsRoots.Add(Path.GetFullPath(directory));
            if (File.Exists(pythonw) && File.Exists(pluginHelper)) env["WECHAT_PLUGIN_RUNTIME_CONFIG"] = Deployment.Json.Serialize(new {
                python = Path.Combine(resources, "windows-python-cli", "python.exe"), pythonw = pythonw,
                windows_helper = pluginHelper, runtime_roots = new [] { Path.GetDirectoryName(pythonw) },
                document_runtime_root = Directory.Exists(documentRuntime) ? documentRuntime : "",
                catalog_directory = Path.Combine(d.Workspace, "plugins"),
                device_tools_roots = deviceToolsRoots.ToArray()
            });
            web = OwnedProcess.Start(d.AdminExe, new [] { "--desktop" }, d.Workspace, env, Path.Combine(d.Root, "logs", "web.log"), job);
        }
        HttpRequestMessage NativeMessage(string path, object body) {
            var request = new HttpRequestMessage(body == null ? HttpMethod.Get : HttpMethod.Post, d.NativeUrl + path);
            request.Headers.Authorization = new System.Net.Http.Headers.AuthenticationHeaderValue("Bearer", File.ReadAllText(Path.Combine(d.Root, "api-token"), Encoding.UTF8).Trim());
            if (body != null) request.Content = new StringContent(Deployment.Json.Serialize(body), Encoding.UTF8, "application/json");
            return request;
        }
        async Task<Dictionary<string, object>> NativeRequest(string path, object body) {
            using (var request = NativeMessage(path, body)) using (var response = await api.SendAsync(request)) {
                response.EnsureSuccessStatusCode(); return Deployment.Json.Deserialize<Dictionary<string, object>>(await response.Content.ReadAsStringAsync());
            }
        }
        internal async Task<int> Pending(CancellationToken cancel) {
            var result = await commands.Run(d.AdminExe, new [] { "desktop-busy", d.Root }, cancel);
            if (result.Code != 0) throw new InvalidOperationException("无法核验微信在途任务，保持服务运行。");
            var data = Deployment.Json.Deserialize<Dictionary<string, object>>(result.Output);
            int pending = Convert.ToInt32(data["pending"]);
            if (pending < 0) throw new InvalidOperationException("在途任务状态无效。");
            return pending;
        }
        internal async Task Stop() {
            // "stopped" is only admissible when this runtime had both of its
            // owned children alive and admitted, then stopped them gracefully.
            // Do not produce cutover evidence for an incomplete startup, an
            // already-dead child, a forced cleanup, or any failed shutdown.
            bool gracefulWeb = false, gracefulNative = false;
            var hadNative = native != null;
            var stoppedServerId = serverId;
            if (web != null) {
                if (web.Alive) {
                    await web.StopWeb();
                    gracefulWeb = web.ExitCode == 0;
                } else await web.WaitStopped();
                web.Dispose(); web = null;
            }
            if (native != null) {
                if (native.Alive && (String.IsNullOrEmpty(serverId) || String.IsNullOrEmpty(generation))) {
                    // Startup failed before admission; this process cannot have
                    // received work through our Web. Only terminate our child.
                    native.StopUnadmitted();
                } else if (native.Alive) {
                    await NativeRequest("/v1/daemon/shutdown", new { pid = native.ID, serverId = serverId, ownershipGeneration = generation });
                    gracefulNative = true;
                }
                await native.WaitStopped();
                gracefulNative = gracefulNative && native.ExitCode == 0;
                native.Dispose(); native = null;
            }
            if (hadNative) await FinishRemovedAccounts(CancellationToken.None);
            if (job != null) { job.Dispose(); job = null; }
            try {
                if (gracefulWeb && gracefulNative && cutoverGate != null)
                    WriteLegacyCutoverState(d.Root, d.Account, "stopped", cutoverEpoch, stoppedServerId);
            } finally {
                if (cutoverGate != null) { cutoverGate.Dispose(); cutoverGate = null; }
            }
        }
        public void Dispose() {
            api.Dispose();
            if (web != null) web.Dispose(); if (native != null) native.Dispose();
            if (job != null) job.Dispose();
            if (cutoverGate != null) cutoverGate.Dispose();
        }
    }

    internal sealed class OwnedProcess : IDisposable {
        readonly Process process;
        readonly StreamWriter log;
        readonly Task output, error;
        readonly object gate = new object();
        internal bool Alive { get { return !process.HasExited; } }
        internal int ID { get { return process.Id; } }
        internal int ExitCode { get { return process.ExitCode; } }
        OwnedProcess(Process child, StreamWriter writer) {
            process = child; log = writer;
            output = Pump(child.StandardOutput); error = Pump(child.StandardError);
        }
        async Task Pump(StreamReader stream) { string line; while ((line = await stream.ReadLineAsync()) != null) lock (gate) { log.WriteLine(DateTime.UtcNow.ToString("o") + " " + line); log.Flush(); } }
        internal static void ClearInheritedEnvironment(ProcessStartInfo info, bool clearStationSecrets) {
            foreach (var name in info.EnvironmentVariables.Keys.Cast<string>().Where(x =>
                x.StartsWith("WECHAT_", StringComparison.OrdinalIgnoreCase) || x.StartsWith("XDG_", StringComparison.OrdinalIgnoreCase) ||
                (clearStationSecrets && x.StartsWith("AHS_", StringComparison.OrdinalIgnoreCase))).ToArray()) info.EnvironmentVariables.Remove(name);
        }
        internal static OwnedProcess Start(string exe, string[] args, string workspace, Dictionary<string, string> environment, string logPath, ProcessJob job, bool clearStationSecrets = false) {
            var info = HiddenRunner.Info(exe, args); info.WorkingDirectory = workspace;
            ClearInheritedEnvironment(info, clearStationSecrets);
            if (environment != null) foreach (var pair in environment) info.EnvironmentVariables[pair.Key] = pair.Value;
            var log = new StreamWriter(new FileStream(logPath, FileMode.Append, FileAccess.Write, FileShare.Read));
            var child = new Process { StartInfo = info };
            bool started = false;
            try { child.Start(); started = true; job.Assign(child); return new OwnedProcess(child, log); }
            catch { if (started && !child.HasExited) child.Kill(); child.Dispose(); log.Dispose(); throw; }
        }
        internal async Task StopWeb() { await process.StandardInput.WriteLineAsync("stop"); await process.StandardInput.FlushAsync(); await WaitStopped(); }
        internal void StopUnadmitted() { if (!process.HasExited) process.Kill(); }
        internal async Task WaitStopped() {
            var deadline = DateTime.UtcNow.AddSeconds(30);
            while (!process.HasExited) {
                if (DateTime.UtcNow > deadline) throw new InvalidOperationException("服务尚未完成安全退出；没有强制结束在途任务。");
                await Task.Delay(100);
            }
            await Task.WhenAll(output, error);
        }
        public void Dispose() { process.Dispose(); lock (gate) log.Dispose(); }
    }

    internal sealed class ProcessJob : IDisposable {
        readonly SafeFileHandle handle;
        [StructLayout(LayoutKind.Sequential)] struct BasicLimits { public long ProcessTime, JobTime; public uint Flags; public UIntPtr MinWorking, MaxWorking; public uint ActiveLimit; public UIntPtr Affinity; public uint Priority, Scheduling; }
        [StructLayout(LayoutKind.Sequential)] struct IOCounters { public ulong ReadOps, WriteOps, OtherOps, ReadBytes, WriteBytes, OtherBytes; }
        [StructLayout(LayoutKind.Sequential)] struct Limits { public BasicLimits Basic; public IOCounters IO; public UIntPtr ProcessMemory, JobMemory, PeakProcess, PeakJob; }
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)] static extern SafeFileHandle CreateJobObject(IntPtr attributes, string name);
        [DllImport("kernel32.dll", SetLastError = true)] static extern bool SetInformationJobObject(SafeFileHandle job, int type, ref Limits limits, uint size);
        [DllImport("kernel32.dll", SetLastError = true)] static extern bool AssignProcessToJobObject(SafeFileHandle job, IntPtr process);
        internal ProcessJob() {
            handle = CreateJobObject(IntPtr.Zero, null);
            var limits = new Limits(); limits.Basic.Flags = 0x2000; // KILL_ON_JOB_CLOSE
            if (handle.IsInvalid || !SetInformationJobObject(handle, 9, ref limits, (uint)Marshal.SizeOf(typeof(Limits)))) { handle.Dispose(); throw new InvalidOperationException("无法建立本实例的进程隔离，未启动服务。"); }
        }
        internal void Assign(Process process) { if (!AssignProcessToJobObject(handle, process.Handle)) throw new InvalidOperationException("无法确认子进程归属，已拒绝启动。"); }
        public void Dispose() { handle.Dispose(); }
    }
}
