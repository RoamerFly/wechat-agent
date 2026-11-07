using System;
using System.Collections;
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

namespace WechatAgent.Desktop {
    // Invoked only by DesktopTests. Both desktop runtimes and both Go Web
    // services are real; the isolated native transport never leaves loopback.
    internal static class CutoverRehearsal {
        const string Account = "synthetic-cutover-account";
        const string OldDataset = "synthetic-old-dataset", NewDataset = "synthetic-new-dataset";
        const string OldKey = "synthetic-old-receipt", NewKey = "synthetic-new-receipt";
        const string Password = "synthetic-station-cutover-password";
        static readonly Encoding Utf8 = new UTF8Encoding(false);

        internal static void Run(string station, string kit) {
            kit = Path.GetFullPath(kit);
            WindowsRuntime.NoLinks(kit);
            ControllerTests.Assert(String.Equals(Path.GetDirectoryName(kit), WindowsRuntime.ComparablePath(Path.GetTempPath()), StringComparison.OrdinalIgnoreCase)
                && Regex.IsMatch(Path.GetFileName(kit), @"\Aahs-wechat-cutover-[a-f0-9]{32}\z"), "cutover rehearsal requires a fresh temporary kit");
            var old = Deployment.WindowsDefault(Path.Combine(kit, "old-workspace"));
            var next = Deployment.LocalStationDefault(Path.Combine(kit, "station-workspace"));
            ControllerTests.Assert(!Directory.Exists(old.Workspace) && !Directory.Exists(next.Workspace), "cutover rehearsal cannot reuse a data root");
            next.StationExe = Path.GetFullPath(station);
            old.AdminExe = Path.Combine(kit, "fixture-admin.exe");
            old.Account = Account;
            var ports = new HashSet<string>();
            Func<string> url = () => { string value; do { value = ControllerTests.CycleUrl(); } while (!ports.Add(value)); return value; };
            old.Url = url(); old.NativeUrl = url(); next.Url = url();
            var stationNativeUrl = url();
            NativeBundle(kit, old.NativeExe, old.AdminExe);
            NativeBundle(kit, next.StationNativeExe, null);
            var oldConfig = Path.Combine(old.Root, "admin", "config.json");
            var oldQueue = Path.Combine(old.Root, "bot-state", "messages-v2.json");
            FixtureRoot(old.Root, OldDataset);
            Write(oldConfig, new { enabled = false, filehelper_enabled = true, provider = "deepseek",
                model = "deepseek-flash", base_url = "https://model.invalid", max_tokens = 2048,
                thinking_enabled = true, reasoning_effort = "low" });
            var oldConfigHash = Sha256(oldConfig);
            WriteCheckpoint(oldQueue, OldDataset, OldKey, "87111");
            // Rehearse the deployed old host shape without a fish identity
            // file. Only model settings move before the first local login.
            Directory.CreateDirectory(Path.GetDirectoryName(next.Root));
            var settingsArgs = new [] { "--data", next.Root, "--legacy-settings-source", Path.GetDirectoryName(oldConfig) };
            var settingsPlan = OfflineCommand(next.StationExe, settingsArgs);
            ControllerTests.Assert(Deployment.Text(settingsPlan, "status") == "preview" && !Convert.ToBoolean(settingsPlan["has_identity"])
                && !Directory.Exists(next.Root), "settings preview without an old fish identity did not create the new host");
            var settingsApply = settingsArgs.Concat(new [] { "--legacy-settings-digest", Deployment.Text(settingsPlan, "digest") }).ToArray();
            ControllerTests.Assert(Deployment.Text(OfflineCommand(next.StationExe, settingsApply), "status") == "applied"
                && !File.Exists(Path.Combine(next.Root, "fish-identity.json")), "settings import did not invent an old fish identity");
            ControllerTests.Assert(Deployment.Text(OfflineCommand(next.StationExe, settingsApply), "status") == "applied",
                "settings import without an old identity can be retried before first use");
            FixtureRoot(next.StationNativeRoot, NewDataset);
            WriteText(Path.Combine(next.Root, "admin-password"), Password);

            using (var legacy = new WindowsRuntime(old, new HiddenRunner()))
            using (var current = new LocalStationRuntime(next))
            using (var client = new HttpClient(new HttpClientHandler { UseProxy = false, AllowAutoRedirect = false })) {
                client.Timeout = TimeSpan.FromSeconds(5);
                const string originalWork = "Synthetic local work before connecting WeChat.";
                const string firstDraft = "连接微信前保留的本地聊天原文";
                const string lastDraft = "断开微信后保留的本地聊天原文";
                current.Start(CancellationToken.None).GetAwaiter().GetResult();
                WaitHealthy(client, next.Url, current.CheckAlive);
                AuthenticateStation(client, next.Url);
                var otherApps = AssertWithoutWeChat(client, next, current);
                ControllerTests.Assert(!Convert.ToBoolean(Read(client, next.Url + "/api/local/home")["initialized"]),
                    "the packaged station waits for the owner to initialize a new fish identity");
                var identity = ControllerTests.StationJSON(client, HttpMethod.Post, next.Url + "/api/local/home", new { action = "initialize" });
                AssertSameHome(client, next.Url, identity);
                AssertImportedModel(client, next.Url);
                SubmitLocalDraft(client, next.Url, "before-connect", firstDraft);
                var work = Path.Combine(Deployment.Text(identity, "directory"), "works", "cutover-fixture.txt");
                WriteText(work, originalWork);
                var artifact = ControllerTests.StationJSON(client, HttpMethod.Post, next.Url + "/api/local/artifacts",
                    new { path = "works/cutover-fixture.txt", expected_sha256 = Sha256(work).ToLowerInvariant(), request_key = "cutover-local-work" });
                var artifactURL = next.Url + "/api/local/artifacts/" + Deployment.Text(artifact, "artifact_id") + "/download";
                WriteText(work, "Edited after registration.");
                AssertLocalContinuity(client, next.Url, identity, artifactURL, originalWork, firstDraft);
                current.Stop().GetAwaiter().GetResult();

                var snapshot = Path.Combine(kit, "legacy-snapshot");
                var snapshotArgs = new [] { "legacy-wechat-snapshot", "--legacy-data", old.Root, "--snapshot", snapshot, "--account", Account };
                StartLegacy(legacy, old, client);
                ExpectRefused(() => OfflineCommand(next.StationExe, snapshotArgs), "旧桌面仍在使用");
                legacy.Stop().GetAwaiter().GetResult();
                var oldMarker = WindowsRuntime.LegacyCutoverStatePath(old.Root);
                var oldProof = File.ReadAllBytes(oldMarker);
                var oldEpoch = StoppedEpoch(oldMarker, OldDataset);
                var snapshotPreview = OfflineCommand(next.StationExe, snapshotArgs);
                ControllerTests.Assert(Deployment.Text(snapshotPreview, "state") == "ready" && !Directory.Exists(snapshot), "offline snapshot preview holds the old lease without creating the destination");
                var captured = OfflineCommand(next.StationExe, snapshotArgs.Concat(new [] { "--apply", Deployment.Text(snapshotPreview, "apply_digest") }).ToArray());
                var verified = OfflineCommand(next.StationExe, new [] { "legacy-wechat-snapshot", "--snapshot", snapshot, "--verify" });
                ControllerTests.Assert(Deployment.Text(captured, "state") == "created" && Deployment.Text(verified, "state") == "verified"
                    && Deployment.Text(captured, "manifest_digest") == Deployment.Text(verified, "manifest_digest"), "the packaged command creates and independently verifies a private snapshot");
                var snapshotQueue = Path.Combine(snapshot, "bot-state", "messages-v2.json");
                var originalQueue = File.ReadAllBytes(snapshotQueue);
                var app = Path.GetDirectoryName(next.StationNativeRoot);
                Write(Path.Combine(app, "legacy-import-source.json"), new { snapshot_root = snapshot, legacy_host_root = old.Root,
                    primary_account = Account, snapshot_stopped = true, snapshot_manifest_digest = Deployment.Text(verified, "manifest_digest") });

                next.StationNativeUrl = stationNativeUrl;
                current.Start(CancellationToken.None).GetAwaiter().GetResult();
                WaitHealthy(client, next.Url, current.CheckAlive);
                AuthenticateStation(client, next.Url);
                AssertLocalContinuity(client, next.Url, identity, artifactURL, originalWork, firstDraft);
                var list = (IList)Read(client, next.Url + "/api/apps/wechat/connections")["items"];
                ControllerTests.Assert(list.Count == 1, "new station sees exactly the synthetic account");
                var connection = Deployment.Text((Dictionary<string, object>)list[0], "id");
                ControllerTests.Assert(Regex.IsMatch(connection, @"\Awechat-[a-z0-9-]+\z"), "new station returned a safe connection ID");
                var profile = Path.Combine(next.StationNativeRoot, "accounts", Account, "profile");
                Directory.CreateDirectory(profile);
                Write(Path.Combine(app, "native-import-" + connection + ".json"), new { id = Account, storageMode = "managed", profilePath = profile });
                var connectionUrl = next.Url + "/api/apps/wechat/connections/" + connection;
                var importUrl = connectionUrl + "/legacy-import";
                var snapshotManifest = Path.Combine(snapshot, "legacy-wechat-snapshot.json");
                var heldManifest = Path.Combine(kit, "held-synthetic-manifest.json");
                File.Move(snapshotManifest, heldManifest);
                try { RejectSnapshotImport(client, HttpMethod.Get, importUrl, null); }
                finally { File.Move(heldManifest, snapshotManifest); }
                var plan = Read(client, importUrl);
                ControllerTests.Assert(Convert.ToInt32(plan["receipt_records"]) == 1 && !Convert.ToBoolean(plan["same_native_dataset"]), "preview preserved the receipt from the old dataset");
                var approvedImport = new { digest = Deployment.Text(plan, "digest") };
                File.AppendAllText(snapshotQueue, "\n", Utf8);
                try { RejectSnapshotImport(client, HttpMethod.Post, importUrl, approvedImport); }
                finally { File.WriteAllBytes(snapshotQueue, originalQueue); }
                ControllerTests.Assert(Convert.ToBoolean(ControllerTests.StationJSON(client, HttpMethod.Post, importUrl, approvedImport)["applied"]), "station applied the read-only old receipt import");
                var oldReceipt = Read(client, connectionUrl + "/receipts/" + OldKey);
                ControllerTests.Assert(Deployment.Text(oldReceipt, "state") == "accepted" && Deployment.Text(oldReceipt, "server_id") == "87111", "station retained the old accepted receipt and server ID");
                var settings = Read(client, next.Url + "/api/local/model");
                settings["api_key"] = "synthetic-loopback-only-model";
                settings["base_url"] = "https://127.0.0.1:1";
                ControllerTests.StationJSON(client, HttpMethod.Put, next.Url + "/api/local/model", settings);
                ControllerTests.StationJSON(client, HttpMethod.Put, connectionUrl + "/policy", new { reply_threads = new [] { "filehelper" } });
                var ungranted = new { artifact_id = Deployment.Text(artifact, "artifact_id"), thread_id = "filehelper",
                    requester_id = "synthetic-member", request_key = "cutover-ungranted", enabled = true };
                using (var request = new StringContent(Deployment.Json.Serialize(ungranted), Utf8, "application/json"))
                using (var response = client.PostAsync(connectionUrl + "/work-grants", request).GetAwaiter().GetResult()) {
                    ControllerTests.Assert(response.StatusCode == HttpStatusCode.Conflict, "text reply grant alone cannot export the local artifact to WeChat");
                }
                File.AppendAllText(snapshotQueue, "\n", Utf8);
                try {
                    var blocked = Read(client, importUrl + "/activation");
                    ControllerTests.Assert(Deployment.Text(blocked, "status") == "blocked" && Deployment.Text(blocked, "reason").Contains("完整性")
                        && !File.Exists(Path.Combine(old.Root, "legacy-cutover-claim.json")), "activation rejects a tampered snapshot before claiming the old host");
                    ControllerTests.Assert(Deployment.Text(Read(client, connectionUrl + "/receipts/" + OldKey), "state") == "accepted", "stored historical receipt remains available without rereading the changed source");
                } finally { File.WriteAllBytes(snapshotQueue, originalQueue); }
                var activation = Read(client, importUrl + "/activation");
                ControllerTests.Assert(Deployment.Text(activation, "status") == "ready", "new station accepted the real old desktop's stop proof: " + Deployment.Text(activation, "reason"));
                var confirmed = ControllerTests.StationJSON(client, HttpMethod.Post, importUrl + "/activation", new { digest = Deployment.Text(activation, "digest"), same_identity = true, reviewed_receipts = true });
                ControllerTests.Assert(Deployment.Text(confirmed, "status") == "confirmed", "station acquired the old desktop claim");
                ControllerTests.Assert(Deployment.Text((Dictionary<string, object>)Read(client, connectionUrl)["connection"], "worker") == "disabled"
                    && !File.Exists(Path.Combine(next.StationNativeRoot, "events-observed")), "confirmation leaves a disabled import dormant until explicit start");
                var claim = Path.Combine(old.Root, "legacy-cutover-claim.json");
                var claimBytes = File.ReadAllBytes(claim);
                ExpectRefused(() => legacy.Start(CancellationToken.None).GetAwaiter().GetResult(), "已经交接");
                ControllerTests.Assert(!legacy.AnyRunning && oldProof.SequenceEqual(File.ReadAllBytes(oldMarker)), "old runtime stays stopped and preserves its proof while claimed");

                var queue = Path.Combine(app, "workers", connection, "queue", "messages-v2.json");
                WriteCheckpoint(queue, NewDataset, NewKey, "87222");
                ControllerTests.StationJSON(client, HttpMethod.Post, connectionUrl + "/lifecycle", new { action = "start" });
                WaitFor(() => Deployment.Text((Dictionary<string, object>)Read(client, connectionUrl)["connection"], "worker") == "running", "activated station starts the worker");
                WaitFor(() => File.Exists(Path.Combine(next.StationNativeRoot, "events-observed")), "station worker polled only the synthetic native event stream");
                var rollback = importUrl + "/rollback";
                ControllerTests.StationJSON(client, HttpMethod.Post, rollback, new { });
                WaitFor(() => Deployment.Text(Read(client, rollback), "state") == "frozen", "station drains the frozen worker");
                var audit = ControllerTests.StationJSON(client, HttpMethod.Post, rollback + "/audit", new { });
                ControllerTests.Assert(Convert.ToBoolean(audit["desktop_bound"]) && Convert.ToInt32(audit["active_count"]) == 0
                    && Convert.ToInt32(audit["unknown_count"]) == 0 && Convert.ToInt32(audit["delivery_count"]) == 1, "audit bound to the running desktop and retained the new receipt");
                var frozen = (IList)Read(client, rollback)["items"];
                ControllerTests.Assert(Deployment.Text((Dictionary<string, object>)frozen[0], "server_id") == "87222", "the frozen report kept the new accepted server receipt");
                var arguments = new [] { "legacy-wechat-release", "--data", next.Root, "--connection", connection, "--audit-digest", Deployment.Text(audit, "digest") };
                ExpectRefused(() => OfflineCommand(next.StationExe, arguments), "仍在使用");

                current.Stop().GetAwaiter().GetResult();
                var newMarker = Path.Combine(next.Root, "station-native-stop.json");
                StoppedEpoch(newMarker, NewDataset);
                var newProof = File.ReadAllBytes(newMarker);
                var preview = OfflineCommand(next.StationExe, arguments);
                var fence = Path.Combine(next.Root, "station-native-release.json");
                ControllerTests.Assert(Deployment.Text(preview, "state") == "ready" && !File.Exists(fence) && claimBytes.SequenceEqual(File.ReadAllBytes(claim)), "offline preview leaves both sides unchanged");
                var digest = Deployment.Text(preview, "apply_digest");
                var apply = arguments.Concat(new [] { "--apply", digest }).ToArray();
                ControllerTests.Assert(Deployment.Text(OfflineCommand(next.StationExe, apply), "state") == "released" && File.Exists(fence) && !File.Exists(claim), "offline apply fences the station and releases the old root");
                var archived = Path.Combine(old.Root, "legacy-cutover-released-" + digest + ".json");
                ControllerTests.Assert(claimBytes.SequenceEqual(File.ReadAllBytes(archived)), "the original claim bytes were archived");
                ExpectRefused(() => current.Start(CancellationToken.None).GetAwaiter().GetResult(), "拒绝重新启动微信原生服务");
                ControllerTests.Assert(current.PID == 0 && current.NativePID == 0 && newProof.SequenceEqual(File.ReadAllBytes(newMarker)), "new runtime cannot start a child or change its stop proof after release");

                next.StationNativeUrl = "";
                current.Start(CancellationToken.None).GetAwaiter().GetResult();
                WaitHealthy(client, next.Url, current.CheckAlive);
                AuthenticateStation(client, next.Url);
                ControllerTests.Assert(otherApps.SequenceEqual(AssertWithoutWeChat(client, next, current)),
                    "the same independently bundled apps remain registered after WeChat release");
                settings = Read(client, next.Url + "/api/local/model");
                settings["base_url"] = "https://model.invalid";
                settings["clear_key"] = true;
                ControllerTests.StationJSON(client, HttpMethod.Put, next.Url + "/api/local/model", settings);
                AssertImportedModel(client, next.Url);
                SubmitLocalDraft(client, next.Url, "after-release", lastDraft);
                AssertLocalContinuity(client, next.Url, identity, artifactURL, originalWork, firstDraft, lastDraft);
                current.Stop().GetAwaiter().GetResult();
                ControllerTests.Assert(newProof.SequenceEqual(File.ReadAllBytes(newMarker)) && File.Exists(fence),
                    "restarting the local station without WeChat keeps the native release fence and clean-stop proof");

                client.DefaultRequestHeaders.Remove("Origin");
                client.DefaultRequestHeaders.Remove("X-CSRF-Token");
                StartLegacy(legacy, old, client);
                legacy.Stop().GetAwaiter().GetResult();
                ControllerTests.Assert(StoppedEpoch(oldMarker, OldDataset) != oldEpoch, "old runtime reacquired its lease and wrote a fresh normal stop proof");
                ControllerTests.Assert(Deployment.Text(OfflineCommand(next.StationExe, apply), "state") == "released", "release remains idempotent after the old host restarted");
                ControllerTests.Assert(oldConfigHash == Sha256(oldConfig)
                    && originalQueue.SequenceEqual(File.ReadAllBytes(snapshotQueue)) && originalQueue.SequenceEqual(File.ReadAllBytes(oldQueue))
                    && File.ReadAllText(queue).Contains(NewKey), "the snapshot and independent old/new receipts survive the return");
                foreach (var root in new [] { old.Root, next.StationNativeRoot })
                    ControllerTests.Assert(!File.Exists(Path.Combine(root, "unexpected-native-write")), "rehearsal performed no native mutation except owned shutdown");
            }
        }

        static void AuthenticateStation(HttpClient client, string url) {
            client.DefaultRequestHeaders.Remove("Origin");
            client.DefaultRequestHeaders.Remove("X-CSRF-Token");
            client.DefaultRequestHeaders.Add("Origin", url);
            var login = ControllerTests.StationJSON(client, HttpMethod.Post, url + "/api/login", new { password = Password });
            client.DefaultRequestHeaders.Add("X-CSRF-Token", Deployment.Text(login, "csrf"));
        }

        static string[] AssertWithoutWeChat(HttpClient client, Deployment deployment, LocalStationRuntime runtime) {
            var apps = ((IList)Read(client, deployment.Url + "/api/apps")["items"]).Cast<Dictionary<string, object>>()
                .Select(app => Deployment.Text(app, "id")).OrderBy(id => id, StringComparer.Ordinal).ToArray();
            using (var response = client.GetAsync(deployment.Url + "/api/apps/wechat/connections").GetAwaiter().GetResult()) {
                ControllerTests.Assert(!apps.Contains("wechat") && response.StatusCode == HttpStatusCode.NotFound && runtime.NativePID == 0,
                    "the desktop without WeChat has no WeChat registration, routes or native child");
            }
            return apps;
        }

        static void AssertSameHome(HttpClient client, string url, Dictionary<string, object> expected) {
            var actual = Read(client, url + "/api/local/home");
            var keys = new [] { "fish_id", "owner_id", "directory" };
            ControllerTests.Assert(keys.All(key => Deployment.Text(expected, key) != "" && Deployment.Text(expected, key) == Deployment.Text(actual, key))
                && Convert.ToBoolean(actual["home_access"]) && Convert.ToBoolean(actual["chat_available"])
                && Convert.ToInt64(expected["revision"]) == Convert.ToInt64(actual["revision"]), "local identity and home authorization survive the app lifecycle");
        }

        static void AssertImportedModel(HttpClient client, string url) {
            var model = Read(client, url + "/api/local/model");
            ControllerTests.Assert(Deployment.Text(model, "model") == "deepseek-flash" && Deployment.Text(model, "base_url") == "https://model.invalid"
                && Convert.ToInt32(model["max_tokens"]) == 2048 && Convert.ToBoolean(model["thinking_enabled"])
                && Deployment.Text(model, "reasoning_effort") == "low" && !Convert.ToBoolean(model["key_configured"]),
                "the local station preserved the imported model and thinking settings with no configured key");
        }

        static void SubmitLocalDraft(HttpClient client, string url, string id, string draft) {
            using (var request = new StringContent(Deployment.Json.Serialize(new { request_id = id, text = draft }), Utf8, "application/json"))
            using (var response = client.PostAsync(url + "/api/local/chat", request).GetAwaiter().GetResult()) {
                var body = response.Content.ReadAsStringAsync().GetAwaiter().GetResult();
                ControllerTests.Assert(response.StatusCode == HttpStatusCode.ServiceUnavailable && body.Contains("没有调用模型"),
                    "chat accepted the local input and reported that the model was unconfigured");
            }
        }

        static void AssertLocalContinuity(HttpClient client, string url, Dictionary<string, object> identity, string artifactURL, string work, params string[] drafts) {
            AssertSameHome(client, url, identity);
            var items = (IList)Read(client, url + "/api/local/chat")["items"];
            var records = items.Cast<Dictionary<string, object>>().ToArray();
            ControllerTests.Assert(records.Length == drafts.Length && records.All(record => Deployment.Text(record, "role") == "user")
                && drafts.All(draft => records.Count(record => Deployment.Text(record, "text") == draft) == 1),
                "each local chat source is retained exactly once without a fabricated reply");
            ControllerTests.Assert(client.GetStringAsync(artifactURL).GetAwaiter().GetResult() == work,
                "local work keeps the registered bytes across connecting and disconnecting the application");
        }

        static void NativeBundle(string kit, string native, string admin) {
            Copy(Path.Combine(kit, "fixture-wx.exe"), native);
            var hash = Sha256(native);
            Write(Path.Combine(Path.GetDirectoryName(native), "windows-native-manifest.json"), new {
                local_station_runtime = admin == null, native_sha256 = hash, admin_sha256 = admin == null ? "" : Sha256(admin),
                files = new Dictionary<string, string> { { "wx.exe", hash } }
            });
        }

        static void FixtureRoot(string root, string dataset) {
            WindowsRuntime.PrivateDirectory(root);
            Write(Path.Combine(root, "cutover-fixture.json"), new { account = Account, dataset });
            WriteText(Path.Combine(root, "api-token"), "synthetic-cutover-native-token");
        }

        static void WriteCheckpoint(string path, string dataset, string key, string receipt) {
            Write(path, new { version = 2, server = dataset, account = Account, deliveries = new [] {
                new { key, id = key + "-job", thread = "filehelper", state = "accepted", server_id = receipt, input = new { account = Account, thread = "filehelper" } }
            } });
        }

        static void StartLegacy(WindowsRuntime runtime, Deployment deployment, HttpClient client) {
            runtime.Start(CancellationToken.None).GetAwaiter().GetResult();
            WaitHealthy(client, deployment.Url, runtime.CheckAlive);
        }

        static string StoppedEpoch(string path, string dataset) {
            var marker = Deployment.Json.Deserialize<Dictionary<string, object>>(File.ReadAllText(path));
            ControllerTests.Assert(Deployment.Text(marker, "state") == "stopped" && Deployment.Text(marker, "native_server_id") == dataset, "normal stop attests the owned native dataset");
            return Deployment.Text(marker, "epoch");
        }

        static Dictionary<string, object> OfflineCommand(string executable, string[] arguments) {
            using (var child = Process.Start(HiddenRunner.Info(executable, arguments))) {
                child.StandardInput.Close();
                var output = child.StandardOutput.ReadToEndAsync(); var error = child.StandardError.ReadToEndAsync();
                if (!child.WaitForExit(30000)) { child.Kill(); child.WaitForExit(); throw new InvalidOperationException("offline command timed out"); }
                if (child.ExitCode != 0) throw new InvalidOperationException(error.GetAwaiter().GetResult().Trim());
                return Deployment.Json.Deserialize<Dictionary<string, object>>(output.GetAwaiter().GetResult());
            }
        }

        static void ExpectRefused(Action action, string reason) {
            try { action(); } catch (InvalidOperationException error) {
                if (error.Message.Contains(reason)) { ControllerTests.Assert(true, reason); return; }
                throw;
            }
            throw new InvalidOperationException("Expected refusal: " + reason);
        }

        static void WaitHealthy(HttpClient client, string url, Action checkAlive) {
            WaitFor(() => {
                checkAlive();
                try { using (var response = client.GetAsync(url + "/healthz").GetAwaiter().GetResult()) return response.StatusCode == HttpStatusCode.OK; }
                catch (HttpRequestException) { return false; }
            }, "owned Web service is healthy");
        }

        static void WaitFor(Func<bool> condition, string reason) {
            var deadline = DateTime.UtcNow.AddSeconds(20);
            while (DateTime.UtcNow < deadline) {
                if (condition()) { ControllerTests.Assert(true, reason); return; }
                Thread.Sleep(100);
            }
            throw new InvalidOperationException("Timed out: " + reason);
        }

        static Dictionary<string, object> Read(HttpClient client, string url) {
            return Deployment.Json.Deserialize<Dictionary<string, object>>(client.GetStringAsync(url).GetAwaiter().GetResult());
        }

        static void RejectSnapshotImport(HttpClient client, HttpMethod method, string url, object payload) {
            using (var request = new HttpRequestMessage(method, url)) {
                if (payload != null) request.Content = new StringContent(Deployment.Json.Serialize(payload), Utf8, "application/json");
                using (var response = client.SendAsync(request).GetAwaiter().GetResult()) {
                    var body = response.Content.ReadAsStringAsync().GetAwaiter().GetResult();
                    ControllerTests.Assert((int)response.StatusCode == 422 && body.Contains("完整性"), "import refused an incomplete or changed pinned snapshot: " + (int)response.StatusCode);
                }
            }
        }

        static void Write(string path, object data) { WriteText(path, Deployment.Json.Serialize(data)); }
        static void WriteText(string path, string data) { Directory.CreateDirectory(Path.GetDirectoryName(path)); File.WriteAllText(path, data, Utf8); }
        static void Copy(string source, string target) { Directory.CreateDirectory(Path.GetDirectoryName(target)); File.Copy(source, target); }
        static string Sha256(string path) { using (var stream = File.OpenRead(path)) using (var sha = SHA256.Create()) return BitConverter.ToString(sha.ComputeHash(stream)).Replace("-", ""); }
    }
}
