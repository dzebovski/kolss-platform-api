-- Route the Kyiv Meta Leads source to the new campaign tab in the existing spreadsheet.
update public.lead_import_sources src
set
  spreadsheet_id = '16Y1Qn7YyILnjVzoscCIxo9ySR0pAC4g8otKpFKe4HOM',
  sheet_name = 'Sheet2',
  header_row = 1
from public.offices office
where src.office_id = office.id
  and office.code = 'kyiv';
