// Fixed agent-terminal ANSI mapping retained from the default terminal palette.
// Product chrome colors come only from tokens.css. No daemon overrides.
import type { ITheme } from "@xterm/xterm";
export const TERMINAL_ANSI: Record<"light" | "dark", ITheme> = {
  "light": {
    "black": "#2E3440",
    "red": "#944049",
    "green": "#51693C",
    "yellow": "#7C5A15",
    "blue": "#426486",
    "magenta": "#7F5478",
    "cyan": "#2D6271",
    "white": "#434C5E",
    "brightBlack": "#000000",
    "brightRed": "#000000",
    "brightGreen": "#000000",
    "brightYellow": "#000000",
    "brightBlue": "#000000",
    "brightMagenta": "#000000",
    "brightCyan": "#000000",
    "brightWhite": "#171A20"
  },
  "dark": {
    "black": "#B4BCC8",
    "red": "#D9B2B9",
    "green": "#A3BE8C",
    "yellow": "#EBCB8B",
    "blue": "#81A1C1",
    "magenta": "#B590AF",
    "cyan": "#90C4D3",
    "white": "#D8DEE9",
    "brightBlack": "#959CA5",
    "brightRed": "#FFFFFF",
    "brightGreen": "#FFFFFF",
    "brightYellow": "#FFFFFF",
    "brightBlue": "#FFFFFF",
    "brightMagenta": "#FFFFFF",
    "brightCyan": "#FFFFFF",
    "brightWhite": "#FFFFFF"
  }
};
