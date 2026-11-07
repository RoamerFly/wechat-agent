using System;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Security.Cryptography;
using System.Text;
using System.Text.RegularExpressions;
using System.Threading;
using System.Threading.Tasks;
using System.Web.Script.Serialization;

namespace WechatAgent.Desktop {
    internal sealed class Deployment {
        internal string Workspace, Distro, User, Root, Account, Music;
        internal string Backend = "wsl", NativeUrl = "http://127.0.0.1:6174";
        internal string NativeExe, AdminExe, StationExe, StationWorkspace, StationNativeExe, StationNativeUrl;
        internal bool Windows { get { return Backend == "windows-native"; } }
        internal bool LocalStation { get { return Backend == "local-station"; } }
        // The local station starts no WeChat-native runtime unless the owner
        // explicitly opts in through wechat_native_url.
        internal bool LocalStationNative { get { return LocalStation && StationNativeUrl != ""; } }
        internal string StationNativeRoot { get { return Path.Combine(Root, "apps", "wechat", "native"); } }
        internal string Url = "http://127.0.0.1:8081";
        internal static readonly JavaScriptSerializer Json = new JavaScriptSerializer { MaxJsonLength = 16 * 1024 * 1024 };
        internal static Deployment Load(string workspace) {
            workspace = Path.GetFullPath(workspace);
            var config = Path.Combine(workspace, "secrets", "local-deployment.json");
            if (!File.Exists(config)) return File.Exists(Path.Combine(workspace, "agent-home-station-server.exe")) ? LocalStationDefault(workspace) : WindowsDefault(workspace);
            var data = Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(config, Encoding.UTF8));
            if (Text(data, "backend") == "local-station") {
                var local = LocalStationDefault(workspace);
                if (Text(data, "runtime_directory") != "") local.StationExe = Path.Combine(Path.GetFullPath(Path.Combine(workspace, Text(data, "runtime_directory"))), "agent-home-station-server.exe");
                if (Text(data, "data_root") != "") {
                    local.Root = Path.GetFullPath(Path.Combine(workspace, Text(data, "data_root")));
                    local.StationWorkspace = Path.Combine(local.Root, "managed-workspaces");
                }
                if (Text(data, "workspace_root") != "") local.StationWorkspace = Path.GetFullPath(Path.Combine(workspace, Text(data, "workspace_root")));
                if (Text(data, "admin_url") != "") local.Url = LocalUrl(Text(data, "admin_url"));
                if (Text(data, "wechat_native_url") != "") local.StationNativeUrl = LocalUrl(Text(data, "wechat_native_url"), true);
                // native_url belonged to the retired all-in-one desktop
                // configuration. Do not silently reinterpret it, otherwise a
                // station can accidentally attach to an unrelated daemon.
                if (Text(data, "native_url") != "" || Text(data, "account") != "" || Text(data, "music_service") != "" || (local.LocalStationNative && new Uri(local.StationNativeUrl).Port == new Uri(local.Url).Port)) throw new InvalidOperationException("本机成长站配置无效：微信原生服务必须使用独立的 wechat_native_url，且不能包含账号或音乐服务参数。");
                return local;
            }
            if (Text(data, "backend") == "windows-native") {
                var win = WindowsDefault(workspace);
                if (Text(data, "runtime_directory") != "") {
                    var runtime = Path.GetFullPath(Path.Combine(workspace, Text(data, "runtime_directory")));
                    win.NativeExe = Path.Combine(runtime, "wx.exe");
                    win.AdminExe = Path.Combine(runtime, "wechat-admin.exe");
                }
                if (Text(data, "data_root") != "") win.Root = Path.GetFullPath(Path.Combine(workspace, Text(data, "data_root")));
                if (Text(data, "account") != "") win.Account = Text(data, "account");
                if (Text(data, "admin_url") != "") win.Url = LocalUrl(Text(data, "admin_url"));
                if (Text(data, "native_url") != "") win.NativeUrl = LocalUrl(Text(data, "native_url"));
                if (!Regex.IsMatch(win.Account, @"\A[A-Za-z0-9_-]+\z") || win.Url == win.NativeUrl || Text(data, "music_service") != "") throw new InvalidOperationException("Windows 原生部署配置无效；本机音乐服务需要独立配置与验收。");
                return win;
            }
            if (Text(data, "backend") != "wsl") throw new InvalidOperationException("未知的本机部署后端。");
            var d = new Deployment { Workspace = workspace, Distro = Text(data, "distro"), User = Text(data, "linux_user"), Root = Text(data, "data_root"), Account = Text(data, "account"), Music = Text(data, "music_service") };
            if (!Regex.IsMatch(d.Distro, @"\A[A-Za-z0-9_.-]+\z") || !Regex.IsMatch(d.User, @"\A[a-z_][a-z0-9_-]*\z") || !Regex.IsMatch(d.Account, @"\A[A-Za-z0-9_-]+\z") || !d.Root.StartsWith("/home/", StringComparison.Ordinal) || d.Root.Split('/').Any(x => x == ".." || x == ".") || d.Root.IndexOfAny(new [] {'\0', '\r', '\n'}) >= 0)
                throw new InvalidOperationException("本机部署配置无效；没有启动或停止服务。");
            if (d.Music != "" && !Regex.IsMatch(d.Music, @"\Awechat-agent-music(?:-[a-f0-9]+)?\.service\z")) throw new InvalidOperationException("受管音乐服务名称无效。");
            return d;
        }
        internal static string LocalUrl(string value, bool explicitPort = false) {
            Uri uri;
            if (!Uri.TryCreate(value, UriKind.Absolute, out uri) || uri.Scheme != "http" || uri.Host != "127.0.0.1" || uri.UserInfo != "" || uri.AbsolutePath != "/" || uri.Query != "" || uri.Fragment != "" || (explicitPort && !Regex.IsMatch(value, @"\Ahttp://127\.0\.0\.1:[1-9][0-9]*\z"))) throw new InvalidOperationException("本机服务地址必须使用 http://127.0.0.1:端口。");
            return explicitPort ? value : uri.GetLeftPart(UriPartial.Authority);
        }
        internal static Deployment WindowsDefault(string workspace) {
            return new Deployment { Workspace = workspace, Backend = "windows-native", Root = Path.Combine(workspace, "data"), Account = "desktop", Music = "", NativeExe = Path.Combine(workspace, "runtimes", "windows-x64", "wx.exe"), AdminExe = Path.Combine(workspace, "runtimes", "windows-x64", "wechat-admin.exe") };
        }
        internal static Deployment LocalStationDefault(string workspace) {
            var data = Path.Combine(workspace, "data", "station");
            return new Deployment { Workspace = workspace, Backend = "local-station", Root = data, StationWorkspace = Path.Combine(data, "managed-workspaces"), StationExe = Path.Combine(workspace, "agent-home-station-server.exe"), StationNativeExe = Path.Combine(workspace, "runtimes", "windows-x64", "wx.exe"), Account = "", Music = "", Url = "http://127.0.0.1:8082", NativeUrl = "", StationNativeUrl = "" };
        }
        // Keep the desktop's local-station login resolution identical to the
        // Go server. The old Windows-native and WSL deployments retain their
        // legacy admin paths in ServiceController.Login below.
        internal static string LocalStationPassword(string root, string explicitPassword) {
            bool found;
            var password = ReadLocalStationPassword(root, "admin-password", out found);
            if (found) return password;
            if (explicitPassword != "") {
                if (!ValidLocalStationPassword(explicitPassword)) throw new InvalidOperationException("本机主人环境变量密码格式无效。");
                return explicitPassword;
            }
            password = ReadLocalStationPassword(root, "admin-token", out found);
            if (found) return password;
            // The Go child creates the bootstrap token before this desktop
            // client logs in. Recreating it here could silently reset the
            // owner credential after a damaged or incomplete data root.
            throw new InvalidOperationException("本机主人凭据文件不可用。");
        }
        static string ReadLocalStationPassword(string root, string name, out bool found) {
            found = false;
            var path = Path.Combine(root, name);
            FileAttributes attributes;
            try { attributes = File.GetAttributes(path); }
            catch (FileNotFoundException) { return ""; }
            catch (DirectoryNotFoundException) { return ""; }
            catch (IOException) { throw new InvalidOperationException("本机主人凭据文件不可用。"); }
            catch (UnauthorizedAccessException) { throw new InvalidOperationException("本机主人凭据文件不可用。"); }
            catch (ArgumentException) { throw new InvalidOperationException("本机主人凭据文件不可用。"); }
            catch (NotSupportedException) { throw new InvalidOperationException("本机主人凭据文件不可用。"); }
            found = true;
            if ((attributes & (FileAttributes.Directory | FileAttributes.Device | FileAttributes.ReparsePoint)) != 0)
                throw new InvalidOperationException("本机主人凭据文件不可用。");
            try {
                // Deliberately decode the raw bytes rather than File.ReadAllText:
                // a UTF-8 BOM is data here, matching Go's strings.TrimSpace path.
                var bytes = new byte[1025];
                int length = 0;
                using (var file = new FileStream(path, FileMode.Open, FileAccess.Read, FileShare.Read, 1025, FileOptions.SequentialScan)) {
                    while (length < bytes.Length) {
                        int read = file.Read(bytes, length, bytes.Length - length);
                        if (read == 0) break;
                        length += read;
                    }
                    if (length > 1024) throw new InvalidOperationException("本机主人凭据文件格式无效。");
                }
                var password = new UTF8Encoding(false, true).GetString(bytes, 0, length).Trim();
                if (!ValidLocalStationPassword(password)) throw new InvalidOperationException("本机主人凭据文件格式无效。");
                return password;
            } catch (InvalidOperationException) { throw; }
            catch (IOException) { throw new InvalidOperationException("本机主人凭据文件不可用。"); }
            catch (UnauthorizedAccessException) { throw new InvalidOperationException("本机主人凭据文件不可用。"); }
            catch (DecoderFallbackException) { throw new InvalidOperationException("本机主人凭据文件格式无效。"); }
            catch (ArgumentException) { throw new InvalidOperationException("本机主人凭据文件不可用。"); }
            catch (NotSupportedException) { throw new InvalidOperationException("本机主人凭据文件不可用。"); }
        }
        static bool ValidLocalStationPassword(string value) {
            if (value == null || value.IndexOfAny(new [] {'\0', '\r', '\n'}) >= 0 || !String.Equals(value, value.Trim(), StringComparison.Ordinal)) return false;
            try { int bytes = new UTF8Encoding(false, true).GetByteCount(value); return bytes >= 5 && bytes <= 128; }
            catch (EncoderFallbackException) { return false; }
        }
        internal static string Text(Dictionary<string, object> data, string key) { object value; return data.TryGetValue(key, out value) && value != null ? Convert.ToString(value) : ""; }
        internal string Identity { get { using (var sha = SHA256.Create()) return BitConverter.ToString(sha.ComputeHash(Encoding.UTF8.GetBytes(Workspace.TrimEnd(Path.DirectorySeparatorChar, Path.AltDirectorySeparatorChar).ToLowerInvariant()))).Replace("-", "").Substring(0, 16); } }
        internal static string FindWorkspace(string[] args) {
            if (args.Length == 2 && args[0] == "--workspace") return Path.GetFullPath(args[1]);
            if (args.Length != 0) throw new ArgumentException("参数：--workspace <项目目录>");
            if (File.Exists(Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "agent-home-station-server.exe")) || File.Exists(Path.Combine(AppDomain.CurrentDomain.BaseDirectory, "runtimes", "windows-x64", "windows-native-manifest.json"))) return AppDomain.CurrentDomain.BaseDirectory;
            for (var dir = new DirectoryInfo(AppDomain.CurrentDomain.BaseDirectory); dir != null; dir = dir.Parent)
                if (File.Exists(Path.Combine(dir.FullName, "secrets", "local-deployment.json"))) return dir.FullName;
            throw new InvalidOperationException("找不到本机部署。请使用桌面快捷方式，或传入 --workspace 项目目录。");
        }
    }

    internal sealed class CommandResult { internal int Code; internal string Output; }
    internal interface ICommandRunner { Task<CommandResult> Run(string file, string[] args, CancellationToken cancel); }
    internal sealed class HiddenRunner : ICommandRunner {
        // Windows CRT quoting; argv never passes through cmd.exe or PowerShell.
        internal static string Quote(string value) {
            if (value.Length > 0 && !value.Any(c => Char.IsWhiteSpace(c) || c == '"')) return value;
            var b = new StringBuilder("\""); int slashes = 0;
            foreach (char c in value) {
                if (c == '\\') { slashes++; continue; }
                if (c == '"') b.Append('\\', slashes * 2 + 1); else b.Append('\\', slashes);
                b.Append(c); slashes = 0;
            }
            b.Append('\\', slashes * 2); return b.Append('"').ToString();
        }
        internal static ProcessStartInfo Info(string file, string[] args) {
            return new ProcessStartInfo(file, String.Join(" ", args.Select(Quote))) { UseShellExecute = false, CreateNoWindow = true, RedirectStandardInput = true, RedirectStandardOutput = true, RedirectStandardError = true, StandardOutputEncoding = Encoding.UTF8, StandardErrorEncoding = Encoding.UTF8 };
        }
        public async Task<CommandResult> Run(string file, string[] args, CancellationToken cancel) {
            using (var p = new Process { StartInfo = Info(file, args) }) {
                p.Start(); p.StandardInput.Close();
                var output = p.StandardOutput.ReadToEndAsync(); var error = p.StandardError.ReadToEndAsync();
                // Observe the real command outcome even if cancellation arrives.
                // Never abandon a systemctl mutation and claim that it was cancelled.
                while (!p.HasExited) await Task.Delay(100);
                var result = new CommandResult { Code = p.ExitCode, Output = await output }; await error;
                return result;
            }
        }
    }

    internal sealed class ServiceController : IDisposable {
        readonly Deployment d;
        readonly ICommandRunner runner;
        readonly HttpClient client;
        readonly CookieContainer cookies = new CookieContainer();
        readonly Func<TimeSpan, CancellationToken, Task> delay;
        Process anchor;
        WindowsRuntime windows;
        LocalStationRuntime station;
        string csrf = "";
        internal Action<string> Progress = delegate { };
        internal ServiceController(Deployment deployment, ICommandRunner commands = null, HttpMessageHandler http = null, Func<TimeSpan, CancellationToken, Task> wait = null) {
            d = deployment; runner = commands ?? new HiddenRunner();
            client = new HttpClient(http ?? new HttpClientHandler { UseProxy = false, CookieContainer = cookies, AllowAutoRedirect = false });
            client.Timeout = TimeSpan.FromSeconds(10); delay = wait ?? Task.Delay;
            if (d.Windows) windows = new WindowsRuntime(d, runner);
            if (d.LocalStation) station = new LocalStationRuntime(d);
        }
        internal CookieCollection SessionCookies() { return cookies.GetCookies(new Uri(d.Url)); }
        internal Task<CommandResult> Wsl(CancellationToken token, params string[] args) {
            var argv = new [] { "-d", d.Distro, "-u", d.User, "--exec" }.Concat(args).ToArray();
            return runner.Run(Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.Windows), "System32", "wsl.exe"), argv, token);
        }
        async Task<string> Checked(CancellationToken token, params string[] args) {
            var r = await Wsl(token, args);
            if (r.Code != 0) throw new InvalidOperationException("本机命令失败（退出码 " + r.Code + "）。请查看 WSL 服务状态；没有删除数据或重试发送。");
            return r.Output.Trim();
        }
        internal async Task<bool> Healthy() {
            try { var r = await Request("/healthz", null); return r.ContainsKey("ok") && Convert.ToBoolean(r["ok"]); } catch (HttpRequestException) { return false; } catch (TaskCanceledException) { return false; }
        }
        async Task<Dictionary<string, object>> Request(string path, object body) {
            using (var request = new HttpRequestMessage(body == null ? HttpMethod.Get : HttpMethod.Post, d.Url + path)) {
                request.Headers.Add("Origin", d.Url);
                if (!d.LocalStation) request.Headers.Add("X-Wechat-Account", d.Account);
                if (csrf != "") request.Headers.Add("X-CSRF-Token", csrf);
                if (body != null) request.Content = new StringContent(Deployment.Json.Serialize(body), Encoding.UTF8, "application/json");
                using (var response = await client.SendAsync(request)) {
                    if (!response.IsSuccessStatusCode) throw new InvalidOperationException("管理服务请求失败（HTTP " + (int)response.StatusCode + "）；没有强行停止服务。");
                    return Deployment.Json.Deserialize<Dictionary<string, object>>(await response.Content.ReadAsStringAsync());
                }
            }
        }
        internal async Task Login(CancellationToken token) {
            string password;
            if (d.LocalStation) {
                password = Deployment.LocalStationPassword(d.Root, Environment.GetEnvironmentVariable("AHS_ADMIN_PASSWORD") ?? "");
            } else if (d.Windows) {
                var path = Path.Combine(d.Root, "admin", "admin-password");
                var fallback = Path.Combine(d.Root, "admin", "admin-token");
                if (!File.Exists(path)) path = fallback;
                password = File.ReadAllText(path, Encoding.UTF8).Trim();
            } else {
                var r = await Wsl(token, "cat", d.Root + "/admin/admin-password");
                if (r.Code == 0) password = r.Output.Trim(); else password = await Checked(token, "cat", d.Root + "/admin/admin-token");
            }
            var login = await Request("/api/login", new { password = password });
            csrf = Deployment.Text(login, "csrf");
            if (d.LocalStation) {
                var runtime = await Request("/api/local/runtime", null);
                if (Deployment.Text(runtime, "kind") != "local-station" || Convert.ToInt32(runtime["pid"]) != station.PID || !String.Equals(WindowsRuntime.ComparablePath(Deployment.Text(runtime, "data_directory")), WindowsRuntime.ComparablePath(d.Root), StringComparison.OrdinalIgnoreCase))
                    throw new InvalidOperationException("本机成长站实例身份不匹配，拒绝接管。");
                return;
            }
            var status = await Request("/api/status", null);
            if (Deployment.Text(status, "account") != d.Account) throw new InvalidOperationException("端口上的账号与部署配置不符；拒绝管理其他实例。");
        }
        internal async Task Start(CancellationToken token) {
            if (d.LocalStation) {
                Progress("正在启动本机成长站…");
                await station.Start(token);
                var until = DateTime.UtcNow.AddSeconds(30);
                while (!await Healthy()) {
                    station.CheckAlive(); token.ThrowIfCancellationRequested();
                    if (DateTime.UtcNow > until) throw new InvalidOperationException("本机成长站未就绪，请检查 data/station/logs/station.log。");
                    await delay(TimeSpan.FromMilliseconds(300), token);
                }
                await Login(token);
                Progress("本机成长站运行中 · 可直接与大肥鱼对话");
                return;
            }
            if (d.Windows) {
                Progress("正在启动 Windows 原生微信后台…");
                await windows.Start(token);
                var until = DateTime.UtcNow.AddSeconds(90);
                while (!await Healthy()) {
                    windows.CheckAlive(); token.ThrowIfCancellationRequested();
                    if (DateTime.UtcNow > until) throw new InvalidOperationException("Windows 管理服务未就绪，请检查 data/logs；登录与数据已保留。");
                    await delay(TimeSpan.FromMilliseconds(500), token);
                }
                await Login(token);
                Progress("Windows 原生服务运行中 · 首次使用请在管理页扫码登录");
                return;
            }
            Progress("正在启动本机服务…"); token.ThrowIfCancellationRequested();
            if (runner is HiddenRunner && (anchor == null || anchor.HasExited)) {
                anchor = new Process { StartInfo = HiddenRunner.Info(Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.Windows), "System32", "wsl.exe"), new [] { "-d", d.Distro, "-u", d.User, "--exec", "/bin/bash", "-c", "exec -a wechat-agent-desktop-anchor-" + d.Identity + " sleep infinity" }) };
                anchor.Start(); anchor.StandardInput.Close(); anchor.BeginOutputReadLine(); anchor.BeginErrorReadLine();
            }
            await Checked(token, "systemctl", "--user", "start", "wechat-bot-native.service", "wechat-bot-web.service");
            if (d.Music != "") await Checked(token, "systemctl", "--user", "start", d.Music);
            var deadline = DateTime.UtcNow.AddSeconds(90);
            while (!await Healthy()) {
                token.ThrowIfCancellationRequested();
                if (DateTime.UtcNow > deadline) throw new InvalidOperationException("服务尚未就绪。配置与登录保留，可重试启动或查看日志。");
                await delay(TimeSpan.FromMilliseconds(800), token);
            }
            await Login(token);
            // Web health can precede native API readiness on a cold boot. Do not
            // misread that temporary false status as a lost login or fail early.
            Progress("管理页已就绪，正在等待微信后台…");
            while (true) {
                token.ThrowIfCancellationRequested();
                var state = await Wsl(token, d.Root + "/bin/wx", "--data-dir", d.Root, "--token-file", d.Root + "/api-token", "--account", d.Account, "--json", "auth", "status");
                if (state.Code == 0) {
                    var native = Deployment.Json.Deserialize<Dictionary<string, object>>(state.Output);
                    if (native == null || !native.ContainsKey("authenticated")) throw new InvalidOperationException("微信后台状态无法核验，请在管理页检查连接。");
                    if (!Convert.ToBoolean(native["authenticated"])) {
                        // Only one idempotent saved-account connect request. A
                        // real login requirement is handled inside the page.
                        var connect = await Wsl(token, d.Root + "/bin/wx", "--data-dir", d.Root, "--token-file", d.Root + "/api-token", "--account", d.Account, "--json", "account", "connect", "--detach");
                        if (connect.Code != 0) Progress("服务已运行，请在管理页检查微信登录状态。");
                    }
                    break;
                }
                if (DateTime.UtcNow > deadline) throw new InvalidOperationException("管理页已启动，但微信后台尚未就绪；登录资料已保留，请重试。");
                await delay(TimeSpan.FromMilliseconds(800), token);
            }
            Progress("服务运行中 · 可在管理页查看微信连接与 Agent 状态");
        }
        // An offline read probe also covers native manual sends and broker jobs
        // not represented by an Agent task. Error/unknown state fails closed.
        internal const string BusyProbe = "import json,sqlite3,sys; from pathlib import Path; root=Path(sys.argv[1]); workspace=Path(sys.argv[2]); n=0\n" +
            "for p in root.glob('accounts/*/agent/control.sqlite'):\n c=sqlite3.connect(p.resolve().as_uri()+'?mode=ro',uri=True); n+=c.execute(\"select count(*) from media_jobs where state in ('queued','running')\").fetchone()[0]; c.close()\n" +
            "for p in (workspace/'pri_workspace').glob('*/music/jobs/jobs.json'):\n data=json.loads(p.read_text()); n+=sum(j.get('state') in ('queued','running') for j in data.values())\n" +
            "print(n)";
        internal async Task Stop(CancellationToken token) {
            if (d.LocalStation) {
                token.ThrowIfCancellationRequested();
                Progress("正在停止本机成长站并保存状态…");
                await station.Stop();
                Progress("已停止 · 大肥鱼的记忆、工作空间与配置已保留");
                return;
            }
            if (d.Windows) { await StopWindows(token); return; }
            string ticket = null; bool webStopped = false; Exception failure = null;
            try {
                if (await Healthy()) {
                    await Login(token);
                    ticket = Deployment.Text(await Request("/api/runtime/drain", new { }), "ticket");
                    if (ticket == "") throw new InvalidOperationException("没有取得安全停止凭据。");
                } else {
                    // A listening-but-unhealthy Web must not be treated as safely
                    // drained, and a failed WSL query must never authorize stop.
                    var web = await Wsl(token, "systemctl", "--user", "is-active", "wechat-bot-web.service");
                    if (web.Code != 3 || web.Output.Trim() != "inactive") throw new InvalidOperationException("管理服务未正常响应且未停止；请恢复服务后重试安全停止。");
                }
                var workspace = await Checked(token, "wslpath", "-a", d.Workspace);
                while (true) {
                    token.ThrowIfCancellationRequested();
                    bool ready = ticket == null || Convert.ToBoolean((await Request("/api/runtime/drain?ticket=" + Uri.EscapeDataString(ticket), null))["ready"]);
                    int busy;
                    if (!Int32.TryParse(await Checked(token, "python3", "-c", BusyProbe, d.Root, workspace), out busy) || busy < 0) throw new InvalidOperationException("无法核验在途任务，保持服务运行。");
                    if (ready && busy == 0) break;
                    Progress("正在等待在途任务结束（附件/音乐 " + busy + "）… 可取消停止");
                    await delay(TimeSpan.FromSeconds(2), token);
                }
                token.ThrowIfCancellationRequested();
                Progress("正在停止本实例的服务并保存状态…");
                await Checked(CancellationToken.None, "systemctl", "--user", "stop", "wechat-bot-web.service"); webStopped = true;
                if (d.Music != "") await Checked(CancellationToken.None, "systemctl", "--user", "stop", d.Music);
                await Checked(CancellationToken.None, "systemctl", "--user", "stop", "wechat-bot-native.service");
                foreach (var unit in new [] { "wechat-bot-web.service", d.Music, "wechat-bot-native.service" }.Where(x => x != "")) {
                    var r = await Wsl(CancellationToken.None, "systemctl", "--user", "is-active", unit);
                    if (r.Code != 3 || r.Output.Trim() != "inactive") throw new InvalidOperationException("部分服务尚未停止；保持窗口打开供你检查。");
                }
                // Only the exact project-owned keepalive argv, including the
                // old manual launcher anchor. Never kill all sleep/wsl processes.
                var anchorStop = await Wsl(CancellationToken.None, "pkill", "-f", "^wechat-agent-(anchor|desktop-anchor)-" + d.Identity + " infinity$");
                if (anchorStop.Code != 0 && anchorStop.Code != 1) throw new InvalidOperationException("服务已停止，但本实例保活进程未能核验退出。");
                StopAnchor(); Progress("已停止 · 微信登录资料、会话和模型已保留");
            } catch (Exception error) { failure = error; }
            if (ticket != null && !webStopped) {
                try { await Request("/api/runtime/undrain", new { ticket = ticket }); }
                catch { Progress("停止未完成；安全等待租约将在 30 秒后自动解除，请检查状态。"); }
            }
            if (failure != null) System.Runtime.ExceptionServices.ExceptionDispatchInfo.Capture(failure).Throw();
        }
        void StopAnchor() { if (anchor != null) { try { if (!anchor.HasExited) anchor.Kill(); } catch (InvalidOperationException) { } anchor.Dispose(); anchor = null; } }
        async Task StopWindows(CancellationToken token) {
            string ticket = null; bool committed = false; Exception failure = null;
            try {
                if (windows.WebRunning) {
                    if (!await Healthy()) throw new InvalidOperationException("管理服务未正常响应，保留服务以免中断在途任务；请查看日志后重试。");
                    await Login(token);
                    ticket = Deployment.Text(await Request("/api/runtime/drain", new { }), "ticket");
                    if (ticket == "") throw new InvalidOperationException("没有取得安全停止凭据。");
                }
                while (windows.AnyRunning) {
                    token.ThrowIfCancellationRequested();
                    bool ready = ticket == null || Convert.ToBoolean((await Request("/api/runtime/drain?ticket=" + Uri.EscapeDataString(ticket), null))["ready"]);
                    int pending = await windows.Pending(token);
                    if (ready && pending == 0) break;
                    Progress("正在等待在途任务结束（微信附件 " + pending + "）… 可取消停止");
                    await delay(TimeSpan.FromSeconds(2), token);
                }
                token.ThrowIfCancellationRequested();
                Progress("正在停止本实例的 Windows 服务并保存状态…"); committed = true;
                await windows.Stop();
                Progress("已停止 · 微信登录资料、会话和模型已保留");
            } catch (Exception error) { failure = error; }
            if (ticket != null && (!committed || windows.WebRunning)) {
                try { await Request("/api/runtime/undrain", new { ticket = ticket }); }
                catch { Progress("停止未完成，安全等待租约将在 30 秒后解除，请检查状态。"); }
            }
            if (failure != null) System.Runtime.ExceptionServices.ExceptionDispatchInfo.Capture(failure).Throw();
        }
        public void Dispose() { client.Dispose(); StopAnchor(); if (windows != null) windows.Dispose(); if (station != null) station.Dispose(); }
    }
}
