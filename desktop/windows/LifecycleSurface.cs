using System;
using System.Diagnostics;
using System.Drawing;
using System.Drawing.Drawing2D;
using System.Runtime.InteropServices;
using System.Windows.Forms;

namespace WechatAgent.Desktop {
    internal sealed class LifecycleSurface : Control {
        readonly Timer frames = new Timer { Interval = 33 };
        readonly Stopwatch clock = new Stopwatch();
        readonly Image emblem;
        string heading = "欢迎回家", detail = "正在准备大肥鱼的工作台…";
        bool running = true, completed;
        [DllImport("user32.dll")] static extern bool SystemParametersInfo(uint action, uint parameter, out bool enabled, uint flags);
        readonly bool motion;
        internal bool Animations { get { return motion; } }
        internal LifecycleSurface() {
            DoubleBuffered = true;
            BackColor = Color.FromArgb(239, 243, 255);
            bool enabled;
            motion = !SystemParametersInfo(0x1042, 0, out enabled, 0) || enabled;
            using (var stream = typeof(LifecycleSurface).Assembly.GetManifestResourceStream("AgentHomeStation.Icon.png")) {
                if (stream != null) using (var original = Image.FromStream(stream)) emblem = new Bitmap(original);
            }
            frames.Tick += delegate {
                float scale = LayoutScale();
                Invalidate(new Rectangle((int)(Width / 2f - 70 * scale), (int)(Height / 2f + 60 * scale), (int)(140 * scale), (int)(14 * scale)));
            };
            VisibleChanged += delegate { UpdateAnimation(); };
        }
        float LayoutScale() { return Math.Min(DeviceDpi / 96f, Math.Min(Width / 300f, Height / 270f)); }
        void UpdateAnimation() {
            if (Visible && running && motion) { clock.Start(); frames.Start(); }
            else frames.Stop();
        }
        internal void State(string title, string message, bool active = true, bool done = false) {
            heading = title; detail = message; running = active; completed = done;
            UpdateAnimation(); Invalidate();
        }
        internal void Detail(string message) { detail = message; Invalidate(); }
        protected override void OnPaint(PaintEventArgs e) {
            base.OnPaint(e);
            var g = e.Graphics;
            g.SmoothingMode = SmoothingMode.AntiAlias;
            g.InterpolationMode = InterpolationMode.HighQualityBicubic;
            float scale = LayoutScale(), dpi = g.DpiX / 96f;
            float x = Width / 2f, y = Height / 2f;
            using (var glow = new LinearGradientBrush(ClientRectangle, Color.FromArgb(239, 243, 255), Color.FromArgb(232, 228, 248), 45f)) g.FillRectangle(glow, ClientRectangle);
            if (emblem != null) g.DrawImage(emblem, x - 44 * scale, y - 104 * scale, 88 * scale, 88 * scale);
            using (var title = new Font(Font.FontFamily, 20 * scale / dpi, FontStyle.Bold))
                TextRenderer.DrawText(g, heading, title, new Rectangle(12, (int)(y - 4 * scale), Math.Max(0, Width - 24), (int)(40 * scale)), Color.FromArgb(35, 43, 81), TextFormatFlags.HorizontalCenter | TextFormatFlags.EndEllipsis);
            using (var body = new Font(Font.FontFamily, 10 * scale / dpi))
                TextRenderer.DrawText(g, detail, body, new Rectangle(18, (int)(y + 84 * scale), Math.Max(0, Width - 36), Math.Max(1, Height - (int)(y + 84 * scale) - 12)), Color.FromArgb(99, 107, 140), TextFormatFlags.HorizontalCenter | TextFormatFlags.WordBreak);
            // This is a waiting pulse, not a fabricated percentage of startup.
            for (int i = 0; i < 3; i++) {
                double phase = clock.Elapsed.TotalSeconds * 3.2 - i * .7;
                int alpha = completed ? 230 : !running || !motion ? 170 : (int)(100 + 130 * (.5 + .5 * Math.Sin(phase)));
                using (var brush = new SolidBrush(Color.FromArgb(alpha, completed ? Color.FromArgb(41, 127, 104) : Color.FromArgb(101, 87, 192))))
                    g.FillEllipse(brush, x + (i - 1) * 18 * scale - 3 * scale, y + 63 * scale, 6 * scale, 6 * scale);
            }
        }
        protected override void Dispose(bool disposing) {
            if (disposing) { frames.Dispose(); if (emblem != null) emblem.Dispose(); }
            base.Dispose(disposing);
        }
    }
}
