package dbusapi

// Introspection fragments, one per interface. Payloads are JSON strings by
// design (see the package comment), so every signature stays shallow.
const (
	xmlManager1 = `  <interface name="io.kleinia.Manager1">
    <property name="Version" type="u" access="read"/>
    <property name="Backend" type="s" access="read"/>
    <property name="HTTPAddr" type="s" access="read"/>
  </interface>`

	xmlSources1 = `  <interface name="io.kleinia.Sources1">
    <method name="ListSources">
      <arg name="sources" direction="out" type="as"/>
    </method>
    <method name="AddSource">
      <arg name="source" direction="in" type="s"/>
      <arg name="id" direction="out" type="u"/>
    </method>
    <method name="UpdateSource">
      <arg name="id" direction="in" type="u"/>
      <arg name="source" direction="in" type="s"/>
    </method>
    <method name="RemoveSource">
      <arg name="id" direction="in" type="u"/>
    </method>
    <method name="SetPriority">
      <arg name="id" direction="in" type="u"/>
      <arg name="priority" direction="in" type="i"/>
    </method>
    <method name="SetWeight">
      <arg name="id" direction="in" type="u"/>
      <arg name="weight" direction="in" type="i"/>
    </method>
    <method name="TestSource">
      <arg name="id" direction="in" type="u"/>
      <arg name="result" direction="out" type="s"/>
    </method>
    <signal name="SourceHealthChanged">
      <arg name="id" type="u"/>
      <arg name="state" type="s"/>
      <arg name="reason" type="s"/>
    </signal>
  </interface>`

	xmlKeys1 = `  <interface name="io.kleinia.Keys1">
    <method name="ListKeys">
      <arg name="keys" direction="out" type="as"/>
    </method>
    <method name="IssueKey">
      <arg name="app" direction="in" type="s"/>
      <arg name="limits" direction="in" type="s"/>
      <arg name="secret" direction="out" type="s"/>
      <arg name="id" direction="out" type="u"/>
    </method>
    <method name="UpdateKey">
      <arg name="name" direction="in" type="s"/>
      <arg name="patch" direction="in" type="s"/>
    </method>
    <method name="SuspendKey">
      <arg name="name" direction="in" type="s"/>
    </method>
    <method name="ResumeKey">
      <arg name="name" direction="in" type="s"/>
    </method>
    <method name="RevokeKey">
      <arg name="name" direction="in" type="s"/>
    </method>
  </interface>`

	xmlModels1 = `  <interface name="io.kleinia.Models1">
    <method name="ListPrices">
      <arg name="models" direction="out" type="as"/>
    </method>
    <method name="SetPrice">
      <arg name="alias" direction="in" type="s"/>
      <arg name="price" direction="in" type="s"/>
    </method>
    <method name="RemovePrice">
      <arg name="alias" direction="in" type="s"/>
    </method>
  </interface>`

	xmlUsage1 = `  <interface name="io.kleinia.Usage1">
    <method name="Summary">
      <arg name="since_rfc3339" direction="in" type="s"/>
      <arg name="aggregates" direction="out" type="as"/>
    </method>
    <method name="Histogram">
      <arg name="since_rfc3339" direction="in" type="s"/>
      <arg name="bucket" direction="in" type="s"/>
      <arg name="dimension" direction="in" type="s"/>
      <arg name="buckets" direction="out" type="as"/>
    </method>
    <method name="Stats">
      <arg name="since_rfc3339" direction="in" type="s"/>
      <arg name="stats" direction="out" type="s"/>
    </method>
    <method name="Recent">
      <arg name="limit" direction="in" type="u"/>
      <arg name="rows" direction="out" type="as"/>
    </method>
    <method name="Logs">
      <arg name="since_rfc3339" direction="in" type="s"/>
      <arg name="offset" direction="in" type="u"/>
      <arg name="limit" direction="in" type="u"/>
      <arg name="filter" direction="in" type="s"/>
      <arg name="rows" direction="out" type="as"/>
      <arg name="total" direction="out" type="u"/>
    </method>
    <method name="KeyWindows">
      <arg name="ident" direction="in" type="s"/>
      <arg name="windows" direction="out" type="s"/>
    </method>
  </interface>`

	xmlQuota1 = `  <interface name="io.kleinia.Quota1">
    <signal name="QuotaWarning">
      <arg name="ident" type="s"/>
      <arg name="window" type="s"/>
      <arg name="pct" type="d"/>
    </signal>
  </interface>`

	xmlProperties = `  <interface name="org.freedesktop.DBus.Properties">
    <method name="Get">
      <arg name="interface" direction="in" type="s"/>
      <arg name="prop" direction="in" type="s"/>
      <arg name="value" direction="out" type="v"/>
    </method>
    <method name="GetAll">
      <arg name="interface" direction="in" type="s"/>
      <arg name="props" direction="out" type="a{sv}"/>
    </method>
    <method name="Set">
      <arg name="interface" direction="in" type="s"/>
      <arg name="prop" direction="in" type="s"/>
      <arg name="value" direction="in" type="v"/>
    </method>
  </interface>`

	xmlIntrospectable = `  <interface name="org.freedesktop.DBus.Introspectable">
    <method name="Introspect">
      <arg name="xml_data" direction="out" type="s"/>
    </method>
  </interface>`

	xmlPeer = `  <interface name="org.freedesktop.DBus.Peer">
    <method name="Ping"/>
    <method name="GetMachineId">
      <arg name="machine_uuid" direction="out" type="s"/>
    </method>
  </interface>`

	xmlDBusShim = `  <interface name="org.freedesktop.DBus">
    <method name="Hello">
      <arg name="unique_name" direction="out" type="s"/>
    </method>
    <method name="RequestName">
      <arg name="name" direction="in" type="s"/>
      <arg name="flags" direction="in" type="u"/>
      <arg name="reply" direction="out" type="u"/>
    </method>
    <method name="ReleaseName">
      <arg name="name" direction="in" type="s"/>
      <arg name="reply" direction="out" type="u"/>
    </method>
    <method name="AddMatch">
      <arg name="rule" direction="in" type="s"/>
    </method>
    <method name="RemoveMatch">
      <arg name="rule" direction="in" type="s"/>
    </method>
    <method name="GetId">
      <arg name="id" direction="out" type="s"/>
    </method>
    <method name="ListNames">
      <arg name="names" direction="out" type="as"/>
    </method>
    <method name="ListActivatableNames">
      <arg name="names" direction="out" type="as"/>
    </method>
    <method name="NameHasOwner">
      <arg name="name" direction="in" type="s"/>
      <arg name="has_owner" direction="out" type="b"/>
    </method>
    <method name="GetNameOwner">
      <arg name="name" direction="in" type="s"/>
      <arg name="owner" direction="out" type="s"/>
    </method>
    <method name="GetConnectionUnixProcessID">
      <arg name="name" direction="in" type="s"/>
      <arg name="pid" direction="out" type="u"/>
    </method>
    <method name="GetConnectionUnixUser">
      <arg name="name" direction="in" type="s"/>
      <arg name="uid" direction="out" type="u"/>
    </method>
  </interface>`
)
