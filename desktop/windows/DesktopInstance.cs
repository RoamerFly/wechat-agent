using System;
using System.Threading;

namespace WechatAgent.Desktop {
    // One desktop application per Windows session, including other copies and
    // --workspace paths. Deployment identity still scopes service ownership.
    internal sealed class DesktopInstance : IDisposable {
        internal const string Name = "Local\\RoamerFly.WechatAgent.Desktop.v2";
        // LocalStation preview is a separate desktop product.  It must never
        // send lifecycle commands to an already-running WeChat desktop window.
        internal const string LocalStationName = "Local\\RoamerFly.AgentHomeStation.Local.v1";
        readonly Mutex mutex;
        internal EventWaitHandle Activation { get; private set; }
        internal EventWaitHandle StartRequested { get; private set; }
        internal EventWaitHandle StopRequested { get; private set; }
        internal EventWaitHandle ExitRequested { get; private set; }
        internal bool IsOwner { get; private set; }
        internal DesktopInstance() : this(Name) { }
        internal DesktopInstance(string name) {
            Activation = new EventWaitHandle(false, EventResetMode.AutoReset, name + ".Activate");
            StartRequested = new EventWaitHandle(false, EventResetMode.AutoReset, name + ".start-services");
            StopRequested = new EventWaitHandle(false, EventResetMode.AutoReset, name + ".stop-services");
            ExitRequested = new EventWaitHandle(false, EventResetMode.AutoReset, name + ".exit-services");
            mutex = new Mutex(false, name);
            try { IsOwner = mutex.WaitOne(0); }
            catch (AbandonedMutexException) { IsOwner = true; }
        }
        internal void ActivateOwner() { if (!IsOwner) Activation.Set(); }
        internal static bool RequestCommand(string command, string name = Name) {
            if (command != "start-services" && command != "stop-services" && command != "exit-services") return false;
            try { using (var signal = EventWaitHandle.OpenExisting(name + "." + command)) { return signal.Set(); } }
            catch (WaitHandleCannotBeOpenedException) { return false; }
        }
        public void Dispose() {
            if (IsOwner) { mutex.ReleaseMutex(); IsOwner = false; }
            mutex.Dispose(); Activation.Dispose(); StartRequested.Dispose(); StopRequested.Dispose(); ExitRequested.Dispose();
        }
    }
}
