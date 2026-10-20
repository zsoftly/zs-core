// Prints how java.util.Properties.load reads each case below, as the JSON
// in java_properties.json. Kill Bill reads a per-tenant plugin
// configuration with Properties.load, and TestParsePropertiesMatchesJava
// holds Core's parser to this table. Regenerate with a JDK 11 or later:
//
//   java JavaPropertiesTable.java > java_properties.json
import java.io.StringReader;
import java.util.Properties;
import java.util.TreeMap;

public class JavaPropertiesTable {
    static final String S = "org.killbill.billing.plugin.stripe.apiKey";
    static final String A = "org.killbill.billing.plugin.adyen.enviroment";

    static final String[] CASES = {
        "a=b", "a = b", "a:b", "a b", "a\tb", "a\fb", "  a=b", "a==b", "a=:b", "a : = b", "a  =  b", "a", "a=", "=b", ":b", " =b",
        "a=b  ", "a=b\\ ", "a \\\n b",
        "# c\na=b", "! c\na=b", "  # c\na=b", "\t! c\na=b", "a=b\n#c\\\nd=e", "#c\\\nd=e", "a=1\\\n#b=2",
        "a=b\\\n  c", "a=b\\\r\n  c", "a=b\\\r  c", "a=b\\\n\nc=d", "a=b\\", "a=b\\\\", "a=b\\\\\nc=d", "\\\n# x",
        "a=b\\\n   \\\n  c", "a=b\\\n   \n  c=d", "a\\\n=b",
        "a=1\rb=2", "a=1\r\nb=2", "a=1\n\rb=2", "a=1\r\rb=2", "\r\n\r\na=1\n\n", "   \n\t\na=1",
        "a=\\t\\n\\r\\f", "a=\\x\\y", "k\\=ey=v", "k\\:ey=v", "k\\ ey=v", "\\u0061=\\u0062", "a=\\u00e9", "a=\\u004C\\u0049VE",
        "a=1\na=2", "a=é", "a=b\\\\\\\nc",
        "a=\\u12", "a=\\uZZZZ", "a=\\u",
        S + "=sk_test_a\n" + "org.killbill.billing.plugin.stripe.api\\u004bey=sk_live_b",
        S + "=sk_test_a\r" + S + "=sk_live_b",
        "org.killbill.billing.plugin.adyen.envir\\u006fment=LIVE",
        A + "=TEST\n\r" + A + "=LIVE",
        S + " : sk_test_a", S + "\\\n  =sk_live_b", "# " + S + "=sk_live_a\\\n" + S + "=sk_test_b",
    };

    public static void main(String[] args) throws Exception {
        StringBuilder out = new StringBuilder("[\n");
        for (int i = 0; i < CASES.length; i++) {
            out.append("  {\"in\": ").append(quote(CASES[i]));
            try {
                Properties p = new Properties();
                p.load(new StringReader(CASES[i]));
                TreeMap<String, String> sorted = new TreeMap<>();
                for (String k : p.stringPropertyNames()) {
                    sorted.put(k, p.getProperty(k));
                }
                out.append(", \"props\": {");
                boolean first = true;
                for (var e : sorted.entrySet()) {
                    out.append(first ? "" : ", ").append(quote(e.getKey())).append(": ").append(quote(e.getValue()));
                    first = false;
                }
                out.append("}");
            } catch (IllegalArgumentException e) {
                out.append(", \"error\": true");
            }
            out.append(i + 1 < CASES.length ? "},\n" : "}\n");
        }
        System.out.print(out.append("]\n"));
    }

    static String quote(String s) {
        StringBuilder b = new StringBuilder("\"");
        for (char c : s.toCharArray()) {
            if (c == '"' || c == '\\') {
                b.append('\\').append(c);
            } else if (c < 0x20 || c > 0x7e) {
                b.append(String.format("\\u%04x", (int) c));
            } else {
                b.append(c);
            }
        }
        return b.append('"').toString();
    }
}
