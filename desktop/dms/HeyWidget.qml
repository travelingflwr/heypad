// The HEY widget for DankMaterialShell's bar. Written by
// `heypad desktop-install`, which fills in heypad's path.
//
// It reads `heypad status --json`, the file `heypad watch` keeps, so it
// costs nothing to poll: the watcher does the network, this just looks.
import QtQuick
import Quickshell
import Quickshell.Io
import qs.Common
import qs.Widgets
import qs.Modules.Plugins
import qs.DCommon.Widgets as DCommon

PluginComponent {
    id: root

    readonly property string heypad: "@HEYPAD@"
    property int newCount: 0
    property var threads: []
    property string checked: ""
    property string problem: ""

    function load(text) {
        try {
            const st = JSON.parse(text);
            root.newCount = st.new || 0;
            root.threads = st.threads || [];
            root.problem = st.error || "";
            root.checked = st.checked ? Qt.formatTime(new Date(st.checked), "h:mm ap") : "";
        } catch (e) {
            root.problem = "heypad status didn't answer";
        }
    }

    function run(args) {
        Quickshell.execDetached([root.heypad].concat(args));
    }

    Process {
        id: statusProc
        command: [root.heypad, "status", "--json"]
        stdout: StdioCollector {
            id: statusOut
            waitForEnd: true
        }
        onExited: exitCode => {
            if (exitCode === 0)
                root.load(statusOut.text);
            else
                root.problem = "heypad isn't installed where the widget expects";
        }
    }

    Timer {
        interval: 10000
        running: true
        repeat: true
        triggeredOnStart: true
        onTriggered: if (!statusProc.running) statusProc.running = true
    }

    pillRightClickAction: () => root.run(["open"])
    popoutWidth: 380

    horizontalBarPill: Component {
        Row {
            spacing: Theme.spacingXS
            anchors.verticalCenter: parent.verticalCenter

            DankIcon {
                name: root.newCount > 0 ? "mark_email_unread" : "mail"
                filled: root.newCount > 0
                size: root.iconSize
                color: root.problem !== "" ? Theme.surfaceVariantText : (root.newCount > 0 ? Theme.primary : Theme.surfaceText)
                anchors.verticalCenter: parent.verticalCenter
            }

            StyledText {
                visible: root.newCount > 0
                text: root.newCount
                font.pixelSize: Theme.barTextSize(root.barThickness, root.barConfig?.fontScale, root.barConfig?.maximizeWidgetText)
                color: Theme.primary
                anchors.verticalCenter: parent.verticalCenter
            }
        }
    }

    verticalBarPill: Component {
        Column {
            spacing: 2
            anchors.horizontalCenter: parent.horizontalCenter

            DankIcon {
                name: root.newCount > 0 ? "mark_email_unread" : "mail"
                filled: root.newCount > 0
                size: root.iconSize
                color: root.newCount > 0 ? Theme.primary : Theme.surfaceText
                anchors.horizontalCenter: parent.horizontalCenter
            }

            StyledText {
                visible: root.newCount > 0
                text: root.newCount
                font.pixelSize: Theme.fontSizeSmall
                color: Theme.primary
                anchors.horizontalCenter: parent.horizontalCenter
            }
        }
    }

    popoutContent: Component {
        PopoutComponent {
            headerText: "HEY"
            detailsText: root.problem !== "" ? root.problem
                : (root.newCount === 0 ? "Nothing new" : root.newCount + " new") + (root.checked ? " · checked " + root.checked : "")
            showCloseButton: true

            Column {
                width: parent.width - Theme.spacingM * 2
                anchors.horizontalCenter: parent.horizontalCenter
                spacing: Theme.spacingS

                Repeater {
                    model: root.threads

                    Rectangle {
                        required property var modelData
                        width: parent.width
                        height: 54
                        radius: Theme.cornerRadius
                        color: rowMouse.containsMouse ? Theme.surfaceContainerHighest : Theme.surfaceContainerHigh

                        Column {
                            anchors.fill: parent
                            anchors.margins: Theme.spacingS
                            anchors.leftMargin: Theme.spacingM
                            spacing: 2

                            StyledText {
                                width: parent.width
                                text: modelData.from
                                font.pixelSize: Theme.fontSizeMedium
                                font.weight: Font.Medium
                                color: Theme.surfaceText
                                elide: Text.ElideRight
                            }
                            StyledText {
                                width: parent.width
                                text: modelData.subject
                                font.pixelSize: Theme.fontSizeSmall
                                color: Theme.surfaceVariantText
                                elide: Text.ElideRight
                            }
                        }

                        MouseArea {
                            id: rowMouse
                            anchors.fill: parent
                            hoverEnabled: true
                            cursorShape: Qt.PointingHandCursor
                            onClicked: {
                                root.run(["open"]);
                                root.closePopout();
                            }
                        }
                    }
                }

                StyledText {
                    visible: root.newCount > root.threads.length
                    text: "and " + (root.newCount - root.threads.length) + " more"
                    font.pixelSize: Theme.fontSizeSmall
                    color: Theme.surfaceVariantText
                }

                Row {
                    spacing: Theme.spacingS

                    DCommon.DButton {
                        text: "Open mail"
                        iconName: "mail"
                        onClicked: {
                            root.run(["open"]);
                            root.closePopout();
                        }
                    }
                    DCommon.DButton {
                        text: "Check now"
                        iconName: "refresh"
                        onClicked: root.run(["status", "--refresh"])
                    }
                }
            }
        }
    }
}
