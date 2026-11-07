using System;
using System.Collections;
using System.Collections.Generic;
using System.Diagnostics;
using System.IO;
using System.Linq;
using System.Net;
using System.Net.Http;
using System.Net.Sockets;
using System.Threading;
using System.Threading.Tasks;

namespace WechatAgent.Desktop {
    internal sealed class FakeCommands : ICommandRunner {
        internal List<string> Calls = new List<string>();
        internal bool Stopped, Busy, ProbeFailure, StopFailure, AuthWarmup;
        public Task<CommandResult> Run(string file, string[] args, CancellationToken unused) {
            var command = String.Join(" ", args.Skip(5)); Calls.Add(command);
            int code = 0; string output = "";
            if (command.StartsWith("cat ")) output = "synthetic-password";
            if (command.Contains("auth status")) { if (AuthWarmup) { AuthWarmup = false; code = 3; } else output = "{\"authenticated\":true}"; }
            if (command.StartsWith("wslpath ")) output = "/mnt/e/synthetic workspace";
            if (command.StartsWith("python3 ")) { output = Busy ? "1" : "0"; if (ProbeFailure) code = 2; }
            if (command.Contains(" stop ")) { if (StopFailure) code = 1; else Stopped = true; }
            if (command.Contains(" is-active ")) { code = Stopped ? 3 : 0; output = Stopped ? "inactive\n" : "active\n"; }
            return Task.FromResult(new CommandResult { Code = code, Output = output });
        }
    }
    internal sealed class FakeWeb : HttpMessageHandler {
        internal List<string> Calls = new List<string>();
        internal int AccountHeaders;
        internal bool Ready = true, WrongAccount, Offline, FailDrain;
        protected override Task<HttpResponseMessage> SendAsync(HttpRequestMessage request, CancellationToken unused) {
            string path = request.RequestUri.PathAndQuery; Calls.Add(path);
            if (request.Headers.Contains("X-Wechat-Account")) AccountHeaders++;
            if (Offline && path == "/healthz") throw new HttpRequestException("offline");
            if (FailDrain && path.StartsWith("/api/runtime/drain")) return Task.FromResult(new HttpResponseMessage(HttpStatusCode.Conflict));
            string body = "{}";
            if (path == "/healthz") body = "{\"ok\":true}";
            if (path == "/api/login") body = "{\"csrf\":\"synthetic-csrf\"}";
            if (path == "/api/status") body = "{\"account\":\"" + (WrongAccount ? "other-account" : "synthetic") + "\",\"native\":{\"authenticated\":true}}";
            if (path == "/api/runtime/drain") body = "{\"ticket\":\"synthetic-ticket\"}";
            if (path.StartsWith("/api/runtime/drain?")) body = "{\"ready\":" + (Ready ? "true" : "false") + "}";
            return Task.FromResult(new HttpResponseMessage(HttpStatusCode.OK) { Content = new StringContent(body) });
        }
    }
    internal static class ControllerTests {
        static int count;
        internal static void Assert(bool condition, string reason) { if (!condition) throw new Exception(reason); count++; }

        internal static string CycleUrl() {
            var listener = new TcpListener(IPAddress.Loopback, 0);
            listener.Start();
            try { return "http://127.0.0.1:" + ((IPEndPoint)listener.LocalEndpoint).Port; }
            finally { listener.Stop(); }
        }

        static bool ProcessAlive(int pid) {
            try { using (var process = Process.GetProcessById(pid)) return !process.HasExited; }
            catch (ArgumentException) { return false; }
        }

        internal static Dictionary<string, object> StationJSON(HttpClient client, HttpMethod method, string url, object payload) {
            using (var request = new HttpRequestMessage(method, url)) {
                request.Content = new StringContent(Deployment.Json.Serialize(payload), System.Text.Encoding.UTF8, "application/json");
                using (var response = client.SendAsync(request).GetAwaiter().GetResult()) {
                    Assert(response.IsSuccessStatusCode, "station request failed: " + method + " " + new Uri(url).AbsolutePath + " " + (int)response.StatusCode);
                    return Deployment.Json.Deserialize<Dictionary<string, object>>(response.Content.ReadAsStringAsync().GetAwaiter().GetResult());
                }
            }
        }

        static void LocalLegacyAdoption(HttpClient client, Deployment deployment, string account, string profileUrl) {
            var source = deployment.Root + "-legacy-copy";
            WindowsRuntime.PrivateDirectory(source);
            Directory.CreateDirectory(Path.Combine(source, "admin"));
            Directory.CreateDirectory(Path.Combine(source, "bot-state"));
            File.WriteAllText(Path.Combine(source, "admin", "config.json"),
                Deployment.Json.Serialize(new { enabled = true, filehelper_enabled = true }));
            var queue = Path.Combine(source, "bot-state", "messages-v2.json");
            var checkpoint = Deployment.Json.Serialize(new {
                version = 1, server = "legacy-synthetic-dataset", account,
                deliveries = new [] { new { key = "legacy-rehearsal-receipt", id = "legacy-native-job", thread = "filehelper",
                    state = "accepted", server_id = "87111", input = new { account, thread = "filehelper" } } }
            });
            File.WriteAllText(queue, checkpoint);
            File.WriteAllText(Path.Combine(Path.GetDirectoryName(deployment.StationNativeRoot), "legacy-import-source.json"),
                Deployment.Json.Serialize(new { snapshot_root = source, primary_account = account, snapshot_stopped = true }));
            var url = profileUrl + "/legacy-import";
            var preview = Deployment.Json.Deserialize<Dictionary<string, object>>(client.GetStringAsync(url).GetAwaiter().GetResult());
            Assert(!Convert.ToBoolean(preview["same_native_dataset"]) && Convert.ToInt32(preview["receipt_records"]) == 1, "legacy preview detected the old data set");
            var result = StationJSON(client, HttpMethod.Post, url, new { digest = Deployment.Text(preview, "digest") });
            Assert(Convert.ToBoolean(result["applied"]), "legacy receipt and paused worker imported");
            var receipt = Deployment.Json.Deserialize<Dictionary<string, object>>(client.GetStringAsync(profileUrl + "/receipts/legacy-rehearsal-receipt").GetAwaiter().GetResult());
            Assert(Deployment.Text(receipt, "state") == "accepted" && Deployment.Text(receipt, "lookup") == "legacy_snapshot", "retained historical receipt scoped to the imported account");
            Assert(File.ReadAllText(queue) == checkpoint, "old snapshot remains unchanged");
        }

        static void LocalNativeCycle(string packageDirectory) {
            const string password = "synthetic-local-native-cycle";
            var priorPassword = Environment.GetEnvironmentVariable("AHS_ADMIN_PASSWORD");
            var d = Deployment.LocalStationDefault(Path.GetFullPath(packageDirectory));
            d.Root = Path.Combine(d.Root, "native-cycle-test-" + Guid.NewGuid().ToString("N"));
            d.StationWorkspace = Path.Combine(d.Root, "managed-workspaces");
            d.Url = CycleUrl();
            do { d.StationNativeUrl = CycleUrl(); } while (d.Url == d.StationNativeUrl);
            Environment.SetEnvironmentVariable("AHS_ADMIN_PASSWORD", password);
            try {
                string priorEpoch = null;
                using (var runtime = new LocalStationRuntime(d)) {
                    for (int round = 0; round < 2; round++) {
                        runtime.Start(CancellationToken.None).GetAwaiter().GetResult();
                        var stopPath = Path.Combine(d.Root, "station-native-stop.json");
                        var starting = Deployment.Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(stopPath));
                        var epoch = Deployment.Text(starting, "epoch");
                        Assert(Convert.ToInt32(starting["version"]) == 1 && Deployment.Text(starting, "state") == "starting", "native launch atomically invalidates prior stop proof");
                        Assert(epoch.Length == 32 && epoch != priorEpoch && Deployment.Text(starting, "data_root") == WindowsRuntime.ComparablePath(d.Root), "native launch writes a fresh private proof epoch");
                        Assert(Deployment.Text(starting, "native_server_id") == "" && Deployment.Text(starting, "native_ownership_generation") == "", "unadmitted launch proof has no native identity");
                        var runLock = Path.Combine(d.Root, "station-native-run.lock");
                        using (var contender = new FileStream(runLock, FileMode.Open, FileAccess.ReadWrite, FileShare.ReadWrite)) {
                            var held = false;
                            try { contender.Lock(0, 1); }
                            catch (IOException) { held = true; }
                            Assert(held, "native-enabled station holds a cross-process data-root lease while running");
                        }
                        var station = runtime.PID;
                        var native = runtime.NativePID;
                        try {
                            Assert(station > 0 && native > 0 && station != native, "station and wx are distinct owned children");
                            using (var client = new HttpClient(new HttpClientHandler { UseProxy = false, AllowAutoRedirect = false })) {
                                client.Timeout = TimeSpan.FromSeconds(3);
                                var ready = false;
                                var deadline = DateTime.UtcNow.AddSeconds(20);
                                do {
                                    runtime.CheckAlive();
                                    try {
                                        using (var health = client.GetAsync(d.Url + "/healthz").GetAwaiter().GetResult()) ready = health.IsSuccessStatusCode;
                                    } catch (HttpRequestException) { }
                                    if (!ready) Thread.Sleep(100);
                                } while (!ready && DateTime.UtcNow < deadline);
                                Assert(ready, "native-enabled station is ready");
                                using (var denied = client.GetAsync(d.Url + "/api/apps").GetAwaiter().GetResult())
                                    Assert(denied.StatusCode == HttpStatusCode.Unauthorized, "native applications require owner login");
                                client.DefaultRequestHeaders.Add("Origin", d.Url);
                                using (var body = new StringContent(Deployment.Json.Serialize(new { password }), System.Text.Encoding.UTF8, "application/json"))
                                using (var login = client.PostAsync(d.Url + "/api/login", body).GetAwaiter().GetResult()) {
                                    Assert(login.IsSuccessStatusCode, "native-enabled station owner login");
                                    var session = Deployment.Json.Deserialize<Dictionary<string, object>>(login.Content.ReadAsStringAsync().GetAwaiter().GetResult());
                                    client.DefaultRequestHeaders.Add("X-CSRF-Token", Deployment.Text(session, "csrf"));
                                }
                                var raw = client.GetStringAsync(d.Url + "/api/apps/wechat/connections").GetAwaiter().GetResult();
                                var result = Deployment.Json.Deserialize<Dictionary<string, object>>(raw);
                                var app = (Dictionary<string, object>)result["application"];
                                Assert(Deployment.Text(app, "registration") == "station-native", "owned native provider registered");
                                Assert(Convert.ToBoolean(app["account_setup"]), "owned native provider supports owner account setup");
                                var items = (IList)result["items"];
                                Assert(items.Count == (round == 0 ? 0 : 2), "isolated native profiles persist across restart");
                                const string connections = "/api/apps/wechat/connections";
                                var keys = new [] { "33333333333343338333333333333333", "44444444444444448444444444444444" };
                                var profiles = new List<string>();
                                foreach (var key in keys) {
                                    using (var payload = new StringContent(Deployment.Json.Serialize(new { request_id = key }), System.Text.Encoding.UTF8, "application/json"))
                                    using (var created = client.PostAsync(d.Url + connections, payload).GetAwaiter().GetResult()) {
                                        Assert(created.IsSuccessStatusCode, "station created or reused the exact native profile: " + (int)created.StatusCode);
                                        var profile = Deployment.Json.Deserialize<Dictionary<string, object>>(created.Content.ReadAsStringAsync().GetAwaiter().GetResult());
                                        Assert(Deployment.Text(profile, "account_id") == "account-" + key, "provisioning returned the exact profile");
                                        var profileUrl = d.Url + connections + "/" + Deployment.Text(profile, "id");
                                        profiles.Add(profileUrl);
                                        var worker = round == 0 ? "disabled" : profiles.Count == 1 ? "paused" : "waiting_login";
                                        Dictionary<string, object> connection;
                                        var workerDeadline = DateTime.UtcNow.AddSeconds(5);
                                        do {
                                            var inspect = client.GetStringAsync(profileUrl).GetAwaiter().GetResult();
                                            var view = Deployment.Json.Deserialize<Dictionary<string, object>>(inspect);
                                            connection = (Dictionary<string, object>)view["connection"];
                                            if (Deployment.Text(connection, "worker") == worker) break;
                                            Thread.Sleep(100);
                                        } while (DateTime.UtcNow < workerDeadline);
                                        Assert(Deployment.Text(connection, "login") == "signed_out" && Deployment.Text(connection, "worker") == worker, "native login remains separate from persisted worker state");
                                    }
                                }
                                var list = Deployment.Json.Deserialize<Dictionary<string, object>>(client.GetStringAsync(d.Url + connections).GetAwaiter().GetResult());
                                Assert(((IList)list["items"]).Count == 2, "repeated requests did not create duplicate accounts");
                                if (round == 0) {
                                    LocalLegacyAdoption(client, d, "account-" + keys[0], profiles[0]);
                                    var settings = Deployment.Json.Deserialize<Dictionary<string, object>>(client.GetStringAsync(d.Url + "/api/local/model").GetAwaiter().GetResult());
                                    settings["api_key"] = "synthetic-controller-worker-key";
                                    settings["base_url"] = "https://127.0.0.1:1";
                                    StationJSON(client, HttpMethod.Put, d.Url + "/api/local/model", settings);
                                    foreach (var profileUrl in profiles) {
                                        StationJSON(client, HttpMethod.Put, profileUrl + "/policy", new { reply_threads = new [] { "filehelper" } });
                                    }
                                    var activation = Deployment.Json.Deserialize<Dictionary<string, object>>(client.GetStringAsync(profiles[0] + "/legacy-import/activation").GetAwaiter().GetResult());
                                    Assert(Deployment.Text(activation, "status") == "blocked", "signed-out legacy worker has no cutover proof");
                                    using (var payload = new StringContent("{\"action\":\"start\"}", System.Text.Encoding.UTF8, "application/json"))
                                    using (var refused = client.PostAsync(profiles[0] + "/lifecycle", payload).GetAwaiter().GetResult())
                                        Assert(refused.StatusCode == HttpStatusCode.Conflict, "imported worker cannot bypass the cutover check");
                                    var started = StationJSON(client, HttpMethod.Post, profiles[1] + "/lifecycle", new { action = "start" });
                                    Assert(Deployment.Text(started, "worker") == "waiting_login", "unrelated worker waits for its own login");
                                    var stopped = StationJSON(client, HttpMethod.Post, profiles[0] + "/lifecycle", new { action = "stop" });
                                    Assert(Deployment.Text(stopped, "worker") == "paused", "pausing one worker preserves the other worker state");
                                }
                            }
                        } finally { runtime.Stop().GetAwaiter().GetResult(); }
                        Assert(!ProcessAlive(station) && !ProcessAlive(native), "both owned children exit after stop");
                        var stopProof = Deployment.Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(stopPath));
                        Assert(stopProof.Count == 7 && Convert.ToInt32(stopProof["version"]) == 1 && Deployment.Text(stopProof, "state") == "stopped" && Deployment.Text(stopProof, "epoch") == epoch, "only the admitted owned pair writes its matching stopped proof schema");
                        Assert(Deployment.Text(stopProof, "native_server_id") != "" && Deployment.Text(stopProof, "native_ownership_generation") != "", "stopped proof retains verified native ownership identity");
                        Assert(!String.IsNullOrEmpty(Deployment.Text(stopProof, "at")), "stopped proof records UTC timestamp");
                        var proofBytes = File.ReadAllText(stopPath);
                        runtime.Stop().GetAwaiter().GetResult();
                        Assert(File.ReadAllText(stopPath) == proofBytes, "a second stop cannot create or replace a stop proof");
                        using (var released = new FileStream(runLock, FileMode.Open, FileAccess.ReadWrite, FileShare.ReadWrite)) {
                            released.Lock(0, 1); released.Unlock(0, 1);
                        }
                        Assert(true, "native data-root lease is released only after owned shutdown completes");
                        priorEpoch = epoch;
                    }
                }
                using (var forced = new LocalStationRuntime(d)) {
                    forced.Start(CancellationToken.None).GetAwaiter().GetResult();
                    var markerPath = Path.Combine(d.Root, "station-native-stop.json");
                    var startingBytes = File.ReadAllText(markerPath);
                    var started = Deployment.Json.Deserialize<Dictionary<string, object>>(startingBytes);
                    Assert(Deployment.Text(started, "state") == "starting" && Deployment.Text(started, "epoch") != priorEpoch, "new launch invalidates the last successful stop proof");
                    using (var child = Process.GetProcessById(forced.PID)) {
                        child.Kill();
                        Assert(child.WaitForExit(5000), "synthetic owned station child was forcibly stopped");
                    }
                    Reject(() => forced.Stop()).GetAwaiter().GetResult();
                    Assert(File.ReadAllText(markerPath) == startingBytes, "forced station exit leaves no usable stopped proof");
                }
                var stopProofBeforeRelease = File.ReadAllText(Path.Combine(d.Root, "station-native-stop.json"));
                var releaseIntent = Path.Combine(d.Root, "station-native-release.json");
                File.WriteAllText(releaseIntent, Deployment.Json.Serialize(new { version = 1, state = "released" }));
                using (var released = new LocalStationRuntime(d)) {
                    Reject(() => released.Start(CancellationToken.None)).GetAwaiter().GetResult();
                    Assert(released.PID == 0 && released.NativePID == 0, "offline release intent refuses native startup before either child exists");
                }
                Assert(File.Exists(releaseIntent), "offline release intent is never cleared by a refused native startup");
                Assert(File.ReadAllText(Path.Combine(d.Root, "station-native-stop.json")) == stopProofBeforeRelease, "offline release intent cannot overwrite the existing stop proof");
            } finally { Environment.SetEnvironmentVariable("AHS_ADMIN_PASSWORD", priorPassword); }
        }

        static Deployment Config() { return new Deployment { Workspace = @"E:\synthetic workspace", Distro = "Synthetic", User = "synthetic", Root = "/home/synthetic/data", Account = "synthetic", Music = "wechat-agent-music.service" }; }
        static async Task Reject(Func<Task> action) { bool failed = false; try { await action(); } catch (Exception) { failed = true; } Assert(failed, "Expected refusal"); }
        static async Task LocalStationCredentials() {
            const string environmentPassword = "synthetic-env-owner-password";
            const string managedPassword = "synthetic-managed-owner-password";
            const string legacyToken = "synthetic-legacy-owner-token";
            var root = Path.Combine(Path.GetTempPath(), "desktop station credential " + Guid.NewGuid().ToString("N"));
            var prior = Environment.GetEnvironmentVariable("AHS_ADMIN_PASSWORD");
            Directory.CreateDirectory(root);
            try {
                // This intentionally uses the actual inherited environment to
                // cover the first-login path before the Go server has written
                // an admin-token.
                Environment.SetEnvironmentVariable("AHS_ADMIN_PASSWORD", environmentPassword);
                Assert(Deployment.LocalStationPassword(root, Environment.GetEnvironmentVariable("AHS_ADMIN_PASSWORD")) == environmentPassword, "fresh station uses the nonempty owner environment exactly");
                var utf8 = new System.Text.UTF8Encoding(false);
                File.WriteAllText(Path.Combine(root, "admin-token"), legacyToken + "\n", utf8);
                Assert(Deployment.LocalStationPassword(root, Environment.GetEnvironmentVariable("AHS_ADMIN_PASSWORD")) == environmentPassword, "owner environment takes precedence over legacy token");
                File.WriteAllText(Path.Combine(root, "admin-password"), managedPassword + "\n", utf8);
                Assert(Deployment.LocalStationPassword(root, Environment.GetEnvironmentVariable("AHS_ADMIN_PASSWORD")) == managedPassword, "managed password takes precedence across restart");
                File.Delete(Path.Combine(root, "admin-password"));
                Assert(Deployment.LocalStationPassword(root, "") == legacyToken, "legacy token remains the final managed fallback");
                File.Delete(Path.Combine(root, "admin-token"));
                await Reject(() => { Deployment.LocalStationPassword(root, ""); return Task.FromResult(0); });
                File.WriteAllText(Path.Combine(root, "admin-password"), new string('x', 1025), utf8);
                await Reject(() => { Deployment.LocalStationPassword(root, environmentPassword); return Task.FromResult(0); });
                File.Delete(Path.Combine(root, "admin-password"));
                Directory.CreateDirectory(Path.Combine(root, "admin-password"));
                await Reject(() => { Deployment.LocalStationPassword(root, environmentPassword); return Task.FromResult(0); });
            } finally {
                Environment.SetEnvironmentVariable("AHS_ADMIN_PASSWORD", prior);
                Directory.Delete(root, true);
            }
        }
        static async Task Tests() {
            Assert(Deployment.LocalUrl("http://127.0.0.1:8081/") == "http://127.0.0.1:8081", "legacy loopback URL accepts a trailing slash");
            Assert(Deployment.LocalUrl("http://127.0.0.1:80", true) == "http://127.0.0.1:80", "station native URL retains the explicit default HTTP port");
            await LocalStationCredentials();
            var controlName = "Local\\WechatAgent.Tests." + Guid.NewGuid().ToString("N");
            using (var instance = new DesktopInstance(controlName)) {
                Assert(DesktopInstance.RequestCommand("stop-services", controlName) && instance.StopRequested.WaitOne(0), "local stop command reaches owner");
                Assert(DesktopInstance.RequestCommand("start-services", controlName) && instance.StartRequested.WaitOne(0), "local start command reaches owner");
                Assert(DesktopInstance.RequestCommand("exit-services", controlName) && instance.ExitRequested.WaitOne(0), "local exit command reaches owner");
                Assert(!DesktopInstance.RequestCommand("unknown", controlName), "unknown lifecycle command refused");
            }
            Assert(!DesktopInstance.RequestCommand("stop-services", controlName), "absent instance is not created by command");
            Assert(HiddenRunner.Quote("plain") == "plain", "plain argv");
            Assert(HiddenRunner.Quote("") == "\"\"", "empty argv");
            Assert(HiddenRunner.Quote("space end\\") == "\"space end\\\\\"", "trailing slash");
            Assert(HiddenRunner.Quote("a\"b") == "\"a\\\"b\"", "embedded quote");
            Assert(HiddenRunner.Quote("$()`&") == "$()`&", "no shell evaluation");
            Assert(WindowsRuntime.ComparablePath(@"\\?\E:\synthetic path\data") == @"E:\synthetic path\data", "Rust canonical Windows paths retain identity");
            var command = new FakeCommands(); var web = new FakeWeb();
            using (var c = new ServiceController(Config(), command, web)) {
                await c.Start(CancellationToken.None);
                Assert(command.Calls.Any(x => x == "systemctl --user start wechat-bot-native.service wechat-bot-web.service"), "start existing units");
                Assert(!command.Calls.Any(x => x.Contains(" connect ")), "already authenticated native login preserved");
                await c.Stop(CancellationToken.None);
                var stops = command.Calls.Where(x => x.Contains(" stop ")).ToArray();
                Assert(stops.SequenceEqual(new [] { "systemctl --user stop wechat-bot-web.service", "systemctl --user stop wechat-agent-music.service", "systemctl --user stop wechat-bot-native.service" }), "stop order");
                Assert(web.Calls.Contains("/api/runtime/drain?ticket=synthetic-ticket"), "must drain before stop");
                Assert(!command.Calls.Any(x => x.Contains("--terminate") || x.Contains("shutdown") || x.Contains("powershell")), "never terminate shared WSL or launch scripts");
            }
            var localWeb = new FakeWeb();
            var localDeployment = Deployment.LocalStationDefault(Path.Combine(Path.GetTempPath(), "desktop station request " + Guid.NewGuid().ToString("N")));
            using (var local = new ServiceController(localDeployment, null, localWeb)) {
                Assert(await local.Healthy(), "local station health request succeeds without a WeChat account");
                Assert(localWeb.AccountHeaders == 0, "local station requests omit the retired WeChat account header");
            }
            command = new FakeCommands { Busy = true }; web = new FakeWeb(); var cancel = new CancellationTokenSource();
            using (var c = new ServiceController(Config(), command, web, (t, ct) => { cancel.Cancel(); ct.ThrowIfCancellationRequested(); return Task.FromResult(0); })) {
                await Reject(() => c.Stop(cancel.Token));
                Assert(!command.Calls.Any(x => x.Contains(" stop ")), "busy cancellation does not stop units");
                Assert(web.Calls.Contains("/api/runtime/undrain"), "cancel resumes work");
            }
            command = new FakeCommands { AuthWarmup = true }; web = new FakeWeb();
            using (var c = new ServiceController(Config(), command, web, (t, ct) => Task.FromResult(0))) {
                await c.Start(CancellationToken.None);
                Assert(command.Calls.Count(x => x.Contains("auth status")) == 2, "cold native readiness retry");
                Assert(!command.Calls.Any(x => x.Contains(" connect ")), "no reconnect during native warmup");
            }
            command = new FakeCommands { ProbeFailure = true }; web = new FakeWeb();
            using (var c = new ServiceController(Config(), command, web)) {
                await Reject(() => c.Stop(CancellationToken.None)); Assert(!command.Calls.Any(x => x.Contains(" stop ")), "unknown busy state refuses stop");
                Assert(web.Calls.Contains("/api/runtime/undrain"), "failed probe releases lease");
            }
            command = new FakeCommands(); web = new FakeWeb { WrongAccount = true };
            using (var c = new ServiceController(Config(), command, web)) {
                await Reject(() => c.Stop(CancellationToken.None)); Assert(!web.Calls.Contains("/api/runtime/drain"), "other account not drained"); Assert(!command.Calls.Any(x => x.Contains(" stop ")), "other account not stopped");
            }
            command = new FakeCommands(); web = new FakeWeb { Offline = true };
            using (var c = new ServiceController(Config(), command, web)) { await Reject(() => c.Stop(CancellationToken.None)); Assert(!command.Calls.Any(x => x.Contains(" stop ")), "unresponsive active web not treated as drained"); }
            command = new FakeCommands(); web = new FakeWeb { FailDrain = true };
            using (var c = new ServiceController(Config(), command, web)) { await Reject(() => c.Stop(CancellationToken.None)); Assert(!command.Calls.Any(x => x.Contains(" stop ")), "drain admission failure refuses stop"); }
            command = new FakeCommands { StopFailure = true }; web = new FakeWeb();
            using (var c = new ServiceController(Config(), command, web)) { await Reject(() => c.Stop(CancellationToken.None)); Assert(command.Calls.Count(x => x.Contains(" stop ")) == 1, "do not stop native after web stop fails"); Assert(web.Calls.Contains("/api/runtime/undrain"), "web stop failure resumes worker"); }
            string root = Path.Combine(Path.GetTempPath(), "desktop synthetic " + Guid.NewGuid()); Directory.CreateDirectory(Path.Combine(root, "secrets"));
            try {
                var path = Path.Combine(root, "secrets", "local-deployment.json");
                File.WriteAllText(path, "{\"backend\":\"wsl\",\"distro\":\"Synthetic\",\"linux_user\":\"synthetic\",\"data_root\":\"/home/synthetic/data\",\"account\":\"synthetic\",\"music_service\":\"unrelated.service\"}");
                await Reject(() => { Deployment.Load(root); return Task.FromResult(0); });
                File.WriteAllText(path, File.ReadAllText(path).Replace("unrelated.service", "wechat-agent-music.service"));
                Assert(Deployment.Load(root).Workspace == Path.GetFullPath(root), "workspace relocation");
            } finally { Directory.Delete(root, true); }
            root = Path.Combine(Path.GetTempPath(), "desktop local station " + Guid.NewGuid()); Directory.CreateDirectory(Path.Combine(root, "secrets"));
            try {
                var config = Path.Combine(root, "secrets", "local-deployment.json");
                File.WriteAllText(config, "{\"backend\":\"local-station\",\"admin_url\":\"http://127.0.0.1:18082\",\"wechat_native_url\":\"http://127.0.0.1:18174\"}");
                var local = Deployment.Load(root);
                Assert(local.LocalStationNative && local.StationNativeUrl.EndsWith(":18174") && local.StationNativeExe == Path.Combine(root, "runtimes", "windows-x64", "wx.exe"), "local station native runtime is explicit and package local");
                Assert(local.StationNativeRoot == Path.Combine(root, "data", "station", "apps", "wechat", "native"), "local station native data is private and separate");
                File.WriteAllText(config, "{\"backend\":\"local-station\",\"admin_url\":\"http://127.0.0.1:18082\",\"wechat_native_url\":\"http://127.0.0.1:18082\"}");
                await Reject(() => { Deployment.Load(root); return Task.FromResult(0); });
                File.WriteAllText(config, "{\"backend\":\"local-station\",\"native_url\":\"http://127.0.0.1:18174\"}");
                await Reject(() => { Deployment.Load(root); return Task.FromResult(0); });
            } finally { Directory.Delete(root, true); }
            root = Path.Combine(Path.GetTempPath(), "desktop windows native " + Guid.NewGuid()); Directory.CreateDirectory(root);
            try {
                var privateDir = Path.Combine(root, "private ownership");
                WindowsRuntime.PrivateDirectory(privateDir);
                Assert(System.Security.Principal.WindowsIdentity.GetCurrent().User.Equals(Directory.GetAccessControl(privateDir).GetOwner(typeof(System.Security.Principal.SecurityIdentifier))), "private directory owner matches native user identity");
                File.WriteAllText(Path.Combine(privateDir, "preserve"), "synthetic");
                WindowsRuntime.PrivateDirectory(privateDir);
                Assert(File.ReadAllText(Path.Combine(privateDir, "preserve")) == "synthetic", "private directory admission preserves existing contents");
                var markerRoot = Path.Combine(root, "cutover evidence");
                WindowsRuntime.PrivateDirectory(markerRoot);
                var epoch = Guid.NewGuid().ToString("N");
                using (var gate = WindowsRuntime.BeginLegacyCutoverRun(markerRoot, "synthetic", epoch)) {
                    await Reject(() => { using (WindowsRuntime.BeginLegacyCutoverRun(markerRoot, "synthetic", Guid.NewGuid().ToString("N"))) { } return Task.FromResult(0); });
                }
                var marker = Deployment.Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(WindowsRuntime.LegacyCutoverStatePath(markerRoot)));
                Assert(Convert.ToInt32(marker["version"]) == 2 && Deployment.Text(marker, "state") == "starting" && Deployment.Text(marker, "epoch") == epoch && Deployment.Text(marker, "account_id") == "synthetic" && Deployment.Text(marker, "data_root") == WindowsRuntime.ComparablePath(markerRoot) && Deployment.Text(marker, "native_server_id") == "", "new desktop launch atomically invalidates an earlier stopped cutover proof");
                WindowsRuntime.WriteLegacyCutoverState(markerRoot, "synthetic", "stopped", epoch, "native-synthetic");
                marker = Deployment.Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(WindowsRuntime.LegacyCutoverStatePath(markerRoot)));
                Assert(Deployment.Text(marker, "state") == "stopped" && Deployment.Text(marker, "epoch") == epoch && Deployment.Text(marker, "native_server_id") == "native-synthetic" && Deployment.Text(marker, "at") != "", "clean desktop stop records the same cutover epoch after owned processes exit");
                var stopBytes = File.ReadAllText(WindowsRuntime.LegacyCutoverStatePath(markerRoot));
                File.WriteAllText(Path.Combine(markerRoot, "legacy-cutover-claim.json"), "synthetic claim");
                await Reject(() => { using (WindowsRuntime.BeginLegacyCutoverRun(markerRoot, "synthetic", Guid.NewGuid().ToString("N"))) { } return Task.FromResult(0); });
                Assert(File.ReadAllText(WindowsRuntime.LegacyCutoverStatePath(markerRoot)) == stopBytes, "a claimed root rejects legacy startup without altering its stopped proof");
                var d = Deployment.Load(root);
                var close = new CloseSettings(root);
                Assert(close.Behavior == "ask", "close choice is asked by default");
                close.Save("tray"); Assert(new CloseSettings(root).Behavior == "tray", "remember tray across restart");
                close.Save("exit"); Assert(new CloseSettings(root).Behavior == "exit", "remember exit across restart");
                Assert(File.Exists(Path.Combine(root, "secrets", "desktop-settings.json.bak")), "previous close choice retained as backup");
                close.Save("ask"); Assert(new CloseSettings(root).Behavior == "ask", "restore close prompt from tray settings");
                await Reject(() => { close.Save("unexpected"); return Task.FromResult(0); });
                Assert(d.Windows && d.Root == Path.Combine(root, "data"), "fresh installation defaults to native Windows");
                Assert(d.Identity == Deployment.Load(root + Path.DirectorySeparatorChar).Identity, "shortcut and direct launch share one instance");
                Assert(d.NativeExe == Path.Combine(root, "runtimes/windows-x64/wx.exe".Replace('/', Path.DirectorySeparatorChar)), "native executable resolved from portable root");
                Directory.CreateDirectory(Path.Combine(root, "secrets"));
                var config = Path.Combine(root, "secrets", "local-deployment.json");
                File.WriteAllText(config, "{\"backend\":\"windows-native\",\"data_root\":\"data with 空格\",\"admin_url\":\"http://127.0.0.1:19081\",\"native_url\":\"http://127.0.0.1:19174\",\"account\":\"synthetic\"}");
                d = Deployment.Load(root);
                Assert(d.Root == Path.Combine(root, "data with 空格") && d.Url.EndsWith(":19081") && d.NativeUrl.EndsWith(":19174"), "portable data and port overrides");
                File.WriteAllText(config, File.ReadAllText(config).Replace("\"backend\":", "\"runtime_directory\":\"separate runtime\",\"backend\":"));
                d = Deployment.Load(root);
                Assert(d.NativeExe == Path.Combine(root, "separate runtime", "wx.exe") && d.AdminExe == Path.Combine(root, "separate runtime", "wechat-admin.exe"), "separate verified runtime directory");
                File.WriteAllText(config, File.ReadAllText(config).Replace("127.0.0.1:19081", "example.com:19081"));
                await Reject(() => { Deployment.Load(root); return Task.FromResult(0); });
                File.WriteAllText(config, "{\"backend\":\"unknown\"}");
                await Reject(() => { Deployment.Load(root); return Task.FromResult(0); });
                d = Deployment.WindowsDefault(root);
                Directory.CreateDirectory(Path.GetDirectoryName(d.NativeExe));
                File.WriteAllText(d.NativeExe, "synthetic executable"); File.WriteAllText(d.AdminExe, "synthetic executable");
                string hash; using (var sha = System.Security.Cryptography.SHA256.Create()) hash = BitConverter.ToString(sha.ComputeHash(File.ReadAllBytes(d.NativeExe))).Replace("-", "");
                File.WriteAllText(Path.Combine(Path.GetDirectoryName(d.NativeExe), "windows-native-manifest.json"), "{\"native_sha256\":\"" + hash + "\",\"admin_sha256\":\"" + hash + "\"}");
                WindowsRuntime.VerifyBundle(d); Assert(true, "bundle checksums match");
                var stationRuntime = Path.Combine(root, "station runtime"); Directory.CreateDirectory(stationRuntime);
                var stationWx = Path.Combine(stationRuntime, "wx.exe"); File.WriteAllText(stationWx, "station native executable");
                string stationHash; using (var sha = System.Security.Cryptography.SHA256.Create()) stationHash = BitConverter.ToString(sha.ComputeHash(File.ReadAllBytes(stationWx))).Replace("-", "");
                File.WriteAllText(Path.Combine(stationRuntime, "windows-native-manifest.json"), "{\"local_station_runtime\":true,\"native_sha256\":\"" + stationHash + "\",\"files\":{\"wx.exe\":\"" + stationHash + "\"}}");
                WindowsRuntime.VerifyNativeBundle(stationWx, true); Assert(true, "station native runtime requires and verifies its isolated file manifest");
                File.WriteAllText(Path.Combine(stationRuntime, "unlisted.dll"), "unexpected");
                await Reject(() => { WindowsRuntime.VerifyNativeBundle(stationWx, true); return Task.FromResult(0); });
                Directory.Delete(stationRuntime, true);
                var resource = Path.Combine(root, "runtimes", "windows-python-cli"); Directory.CreateDirectory(resource);
                File.WriteAllText(Path.Combine(resource, "python.exe"), "synthetic executable");
                File.WriteAllText(Path.Combine(resource, "runtime-manifest.json"), "{\"files\":{\"python.exe\":\"" + hash + "\"}}");
                WindowsRuntime.VerifyBundle(d); Assert(true, "project resource hash matches");
                File.AppendAllText(Path.Combine(resource, "python.exe"), "tamper");
                await Reject(() => { WindowsRuntime.VerifyBundle(d); return Task.FromResult(0); });
                File.WriteAllText(Path.Combine(resource, "runtime-manifest.json"), "{\"files\":{\"../../escape.exe\":\"" + hash + "\"}}");
                await Reject(() => { WindowsRuntime.VerifyBundle(d); return Task.FromResult(0); });
                Directory.Delete(resource, true);
                File.AppendAllText(d.NativeExe, "tamper");
                await Reject(() => { WindowsRuntime.VerifyBundle(d); return Task.FromResult(0); });
                using (var job = new ProcessJob()) {
                    var child = Process.Start(HiddenRunner.Info(System.Reflection.Assembly.GetExecutingAssembly().Location, new [] { "--synthetic-child" }));
                    job.Assign(child); job.Dispose();
                    Assert(child.WaitForExit(5000), "owned job closes its process without taskkill or WSL"); child.Dispose();
                }
            } finally { Directory.Delete(root, true); }
            root = Path.Combine(Path.GetTempPath(), "desktop 合成 probe " + Guid.NewGuid()); Directory.CreateDirectory(root);
            try {
                var real = new HiddenRunner();
                string setup = "from pathlib import Path; import sqlite3,sys; p=Path(sys.argv[1]); a=p/'accounts/synthetic/agent'; a.mkdir(parents=True); c=sqlite3.connect(a/'control.sqlite'); c.execute('create table media_jobs(state text)'); c.executemany('insert into media_jobs values (?)',[('running',),('accepted',)]); c.commit(); c.close(); m=p/'pri_workspace/synthetic/music/jobs'; m.mkdir(parents=True); (m/'jobs.json').write_text('{\"a\":{\"state\":\"queued\"},\"b\":{\"state\":\"completed\"}}')";
                var prepared = await real.Run("python", new [] { "-c", setup, root }, CancellationToken.None);
                Assert(prepared.Code == 0, "synthetic probe setup");
                var probe = await real.Run("python", new [] { "-c", ServiceController.BusyProbe, root, root }, CancellationToken.None);
                Assert(probe.Code == 0 && probe.Output.Trim() == "2", "real native/music busy probe on unicode path");
                File.WriteAllText(Path.Combine(root, "pri_workspace/synthetic/music/jobs/jobs.json"), "invalid json");
                probe = await real.Run("python", new [] { "-c", ServiceController.BusyProbe, root, root }, CancellationToken.None);
                Assert(probe.Code != 0, "corrupt music status cannot authorize stop");
            } finally { Directory.Delete(root, true); }
        }
        static int Main(string[] args) { try {
            Console.OutputEncoding = new System.Text.UTF8Encoding(false);
            // Only this test executable implements these synthetic server
            // responses; the shipped Go executable uses its real SQLite probe.
            if (args.Length == 2 && args[0] == "--prepare") {
                WindowsRuntime.PrivateDirectory(Path.Combine(Path.GetFullPath(args[1]), "secrets"));
                return 0;
            }
            if (args.Length == 1 && args[0] == "--desktop") return 23;
            if (args.Length == 2 && args[0] == "desktop-busy") { Console.WriteLine("{\"pending\":0}"); return 0; }
            if (args.Length == 2 && args[0] == "--failure-cycle") {
                var config = Deployment.Load(args[1]);
                if (!config.Windows) throw new Exception("failure cycle requires isolated native deployment");
                using (var c = new ServiceController(config)) {
                    for (int i = 0; i < 2; i++) {
                        bool exited = false;
                        try { c.Start(CancellationToken.None).GetAwaiter().GetResult(); }
                        catch (InvalidOperationException e) { exited = e.Message.Contains("提前退出"); }
                        Assert(exited, "early Web exit must remain recoverable without leaked log handles");
                        c.Stop(CancellationToken.None).GetAwaiter().GetResult();
                    }
                }
                Console.WriteLine("Native Windows failed-start recovery passed twice."); return 0;
            }
            if (args.Length == 1 && args[0] == "--synthetic-child") { Thread.Sleep(60000); return 0; }
            if (args.Length == 3 && args[0] == "--wechat-cutover-rehearsal") {
                CutoverRehearsal.Run(args[1], args[2]);
                Console.WriteLine("Windows old-to-station-to-old cutover rehearsal passed (" + count + " assertions, synthetic transport only).");
                return 0;
            }
            if (args.Length == 2 && args[0] == "--local-native-cycle") {
                LocalNativeCycle(args[1]);
                Console.WriteLine("LocalStation owned wx and Go service passed two real cycles; synthetic legacy receipt, profiles and independent worker states verified, no login or send.");
                return 0;
            }
            if (args.Length == 2 && args[0] == "--local-cycle") {
                const string environmentPassword = "synthetic-real-cycle-env-password";
                const string managedPassword = "synthetic-real-cycle-managed-password";
                var priorPassword = Environment.GetEnvironmentVariable("AHS_ADMIN_PASSWORD");
                var d = Deployment.LocalStationDefault(Path.GetFullPath(args[1]));
                d.Root = Path.Combine(d.Root, "credential-cycle-" + Guid.NewGuid().ToString("N"));
                d.StationWorkspace = Path.Combine(d.Root, "managed-workspaces");
                try {
                    // The first real desktop login has no credential file and
                    // therefore proves that the inherited environment reaches
                    // both the Go child and ServiceController.Login. Before
                    // the restart, install the legacy token plus managed
                    // password; the managed value must win over that same env.
                    Environment.SetEnvironmentVariable("AHS_ADMIN_PASSWORD", environmentPassword);
                    Assert(!d.LocalStationNative, "default local station leaves wx opt-in disabled");
                    var reserved = new TcpListener(IPAddress.Loopback, new Uri(d.Url).Port);
                    try {
                        reserved.Start();
                        using (var blocked = new ServiceController(d)) Reject(() => blocked.Start(CancellationToken.None)).GetAwaiter().GetResult();
                    } finally { reserved.Stop(); }
                    using (var c = new ServiceController(d)) {
                        for (int i = 0; i < 2; i++) {
                            c.Start(CancellationToken.None).GetAwaiter().GetResult();
                            Assert(c.Healthy().GetAwaiter().GetResult(), "local station health after start");
                            c.Stop(CancellationToken.None).GetAwaiter().GetResult();
                            if (i == 0) {
                                Assert(!File.Exists(Path.Combine(d.Root, "admin-token")) && !File.Exists(Path.Combine(d.Root, "admin-password")), "env-only first login did not create or replace a managed credential");
                                var utf8 = new System.Text.UTF8Encoding(false);
                                File.WriteAllText(Path.Combine(d.Root, "admin-token"), "synthetic-real-cycle-legacy-token\n", utf8);
                                File.WriteAllText(Path.Combine(d.Root, "admin-password"), managedPassword + "\n", utf8);
                            }
                        }
                    }
                    Console.WriteLine("LocalStation real ServiceController login passed env-only then managed-password precedence across restart; wx opt-in remained disabled."); return 0;
                } finally { Environment.SetEnvironmentVariable("AHS_ADMIN_PASSWORD", priorPassword); }
            }
            if (args.Length == 2 && args[0] == "--cycle") {
                var config = Deployment.Load(args[1]);
                if (!config.Windows) throw new Exception("cycle requires isolated Windows native deployment");
                using (var c = new ServiceController(config)) {
                    c.Progress = Console.WriteLine;
                    for (int i = 0; i < 2; i++) { c.Start(CancellationToken.None).GetAwaiter().GetResult(); c.Stop(CancellationToken.None).GetAwaiter().GetResult(); }
                }
                Console.WriteLine("Native Windows desktop cycle passed twice; no login or message send."); return 0;
            }
            if (args.Length == 2 && (args[0] == "--probe" || args[0] == "--start" || args[0] == "--stop")) {
                var d = Deployment.Load(args[1]);
                using (var c = new ServiceController(d)) {
                    if (args[0] == "--start" || args[0] == "--stop") {
                        c.Progress = Console.WriteLine;
                        if (args[0] == "--start") c.Start(CancellationToken.None).GetAwaiter().GetResult();
                        else c.Stop(CancellationToken.None).GetAwaiter().GetResult();
                        return 0;
                    }
                    var workspace = c.Wsl(CancellationToken.None, "wslpath", "-a", d.Workspace).GetAwaiter().GetResult();
                    if (workspace.Code != 0) throw new Exception("wslpath failed");
                    var probe = c.Wsl(CancellationToken.None, "python3", "-c", ServiceController.BusyProbe, d.Root, workspace.Output.Trim()).GetAwaiter().GetResult();
                    Console.WriteLine("Probe exit=" + probe.Code + " count=" + probe.Output.Trim()); return probe.Code;
                }
            }
            Tests().GetAwaiter().GetResult(); Console.WriteLine("Desktop controller assertions passed: " + count); return 0;
        } catch (Exception e) { Console.Error.WriteLine(e); return 1; } }
    }
}
