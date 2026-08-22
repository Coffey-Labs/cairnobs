# "dashboard_id/panel_id" -- a bare panel id isn't enough to import
# from, since Read needs the parent dashboard_id to know where to look
# (there's no standalone GET for a single panel).
terraform import cairnobs_dashboard_panel.error_rate <dashboard-id>/<panel-id>
