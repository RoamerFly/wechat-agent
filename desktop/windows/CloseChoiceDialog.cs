using System;
using System.Drawing;
using System.Windows.Forms;

namespace WechatAgent.Desktop {
    internal sealed class CloseChoiceDialog : Form {
        readonly CheckBox remember = new CheckBox();
        internal string Choice { get; private set; }
        internal bool Remember { get { return remember.Checked; } }
        readonly bool dark;
        internal CloseChoiceDialog(bool dark = false) {
            this.dark = dark;
            Choice = "cancel";
            Text = "大肥鱼的家";
            Font = new Font("Microsoft YaHei UI", 10);
            AutoScaleDimensions = new SizeF(96, 96);
            AutoScaleMode = AutoScaleMode.Dpi;
            ClientSize = new Size(448, 296);
            FormBorderStyle = FormBorderStyle.FixedDialog;
            MaximizeBox = MinimizeBox = false;
            ShowInTaskbar = false;
            StartPosition = FormStartPosition.CenterParent;
            BackColor = dark ? Color.FromArgb(25, 31, 58) : Color.FromArgb(247, 248, 255);
            ForeColor = dark ? Color.FromArgb(238, 240, 255) : Color.FromArgb(30, 38, 73);
            var layout = new TableLayoutPanel { Dock = DockStyle.Fill, Padding = new Padding(24), ColumnCount = 1, RowCount = 5 };
            layout.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
            foreach (int height in new[] { 42, 62, 48, 48, 44 }) layout.RowStyles.Add(new RowStyle(SizeType.Absolute, height));
            var heading = new Label { Text = "关闭窗口", Dock = DockStyle.Fill, Font = new Font(Font.FontFamily, 18, FontStyle.Bold), Margin = Padding.Empty };
            var description = new Label { Text = "收起后，大肥鱼会继续运行。\n退出会等待在途任务结束，再停止服务。", Dock = DockStyle.Fill, Margin = Padding.Empty,
                ForeColor = dark ? Color.FromArgb(183, 192, 224) : Color.FromArgb(85, 96, 130) };
            remember.Text = "记住这次选择";
            remember.Dock = DockStyle.Fill;
            remember.Margin = Padding.Empty;
            remember.TabIndex = 2;
            var tray = ChoiceButton("收起到托盘", "tray", 0);
            var exit = ChoiceButton("退出并停止服务", "exit", 1);
            var cancel = ChoiceButton("取消", "cancel", 3);
            var footer = new TableLayoutPanel { Dock = DockStyle.Fill, ColumnCount = 2, RowCount = 1, Margin = new Padding(0, 5, 0, 0) };
            footer.ColumnStyles.Add(new ColumnStyle(SizeType.Percent, 100));
            footer.ColumnStyles.Add(new ColumnStyle(SizeType.Absolute, 88));
            footer.Controls.Add(remember, 0, 0);
            footer.Controls.Add(cancel, 1, 0);
            layout.Controls.Add(heading, 0, 0);
            layout.Controls.Add(description, 0, 1);
            layout.Controls.Add(tray, 0, 2);
            layout.Controls.Add(exit, 0, 3);
            layout.Controls.Add(footer, 0, 4);
            Controls.Add(layout);
            CancelButton = cancel;
            AcceptButton = cancel;
            Shown += delegate {
                // WinForms can scale fonts and table rows differently when a
                // WebView owner moves between DPI settings. Measure the actual
                // fonts after handle creation before centering the dialog.
                float scale = Math.Max(DeviceDpi / 96f, Font.GetHeight() / Font.GetHeight(96f));
                var available = Screen.FromControl(Owner ?? this).WorkingArea;
                ClientSize = new Size(Math.Min((int)(448 * scale), available.Width - (int)(32 * scale)), ClientSize.Height);
                layout.Padding = new Padding((int)(24 * scale));
                footer.ColumnStyles[1].Width = (int)(88 * scale);
                layout.PerformLayout();
                int contentWidth = layout.ClientSize.Width - layout.Padding.Horizontal;
                var limits = new Size(contentWidth, int.MaxValue);
                layout.RowStyles[0].Height = TextRenderer.MeasureText(heading.Text, heading.Font, limits, TextFormatFlags.WordBreak).Height + (int)(10 * scale);
                layout.RowStyles[1].Height = TextRenderer.MeasureText(description.Text, description.Font, limits, TextFormatFlags.WordBreak).Height + (int)(14 * scale);
                layout.RowStyles[2].Height = Math.Max(TextRenderer.MeasureText(tray.Text, tray.Font).Height + (int)(22 * scale), (int)(48 * scale));
                layout.RowStyles[3].Height = layout.RowStyles[2].Height;
                layout.RowStyles[4].Height = Math.Max(TextRenderer.MeasureText(remember.Text, remember.Font).Height + (int)(18 * scale), (int)(44 * scale));
                int height = layout.Padding.Vertical;
                foreach (RowStyle row in layout.RowStyles) height += (int)Math.Ceiling(row.Height);
                ClientSize = new Size(ClientSize.Width, height);
                layout.PerformLayout();
                if (Owner != null && Owner.WindowState != FormWindowState.Minimized) {
                    var bounds = Owner.Bounds;
                    var area = Screen.FromControl(Owner).WorkingArea;
                    Location = new Point(
                        Math.Max(area.Left, Math.Min(area.Right - Width, bounds.Left + (bounds.Width - Width) / 2)),
                        Math.Max(area.Top, Math.Min(area.Bottom - Height, bounds.Top + (bounds.Height - Height) / 2)));
                }
                cancel.Focus();
            };
        }
        Button ChoiceButton(string text, string choice, int tab) {
            var button = new Button { Text = text, AccessibleName = text, Dock = DockStyle.Fill, Margin = new Padding(0, 0, 0, 7), TabIndex = tab,
                UseVisualStyleBackColor = false, FlatStyle = FlatStyle.Flat,
                BackColor = choice == "tray" ? Color.FromArgb(79, 73, 171) : dark ? Color.FromArgb(38, 46, 81) : Color.FromArgb(235, 237, 253),
                ForeColor = choice == "tray" ? Color.White : ForeColor };
            button.FlatAppearance.BorderColor = choice == "tray" ? button.BackColor : dark ? Color.FromArgb(76, 83, 124) : Color.FromArgb(208, 213, 241);
            button.Click += delegate {
                Choice = choice;
                DialogResult = choice == "cancel" ? DialogResult.Cancel : DialogResult.OK;
                Close();
            };
            return button;
        }
    }
}
